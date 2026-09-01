//! Port of `opendaisugi/shell_decompose.py`: fail-closed compound-shell
//! decomposition with tree-sitter-bash, the oracle's own parser, at the
//! version the oracle pins.
//!
//! The port follows the oracle step for step: the parse-error check, the
//! G-4 repair of fused newlines (rewrite to `;`, re-parse, recurse), the
//! walk that collects heads, commands and redirect targets, and every
//! refusal with the oracle's own reason text (Python's `repr`).
//!
//! The walks here are loops, not recursion, so no input can overflow the
//! stack. The oracle's walks are recursive Python functions, and Python
//! raises RecursionError once a call would put more than 1000 frames on
//! the stack. `decompose_py` counts the frames each of the oracle's walks
//! would use and raises that error where the oracle would. The caller
//! passes the frames already on the stack at its call, which differ by
//! call site.

use crate::gate::py::text::{encode_utf8_strict, repr};
use crate::gate::py::PyErr;
use std::collections::{BTreeSet, HashSet};
use tree_sitter::{Node, Parser, Tree};

const WRITE_REDIRECT_OPS: &[&str] = &[">", ">>", "&>", "&>>", ">|", ">&"];
const READ_REDIRECT_OPS: &[&str] = &["<", "<&"];
const FD_CLOSE_OPS: &[&str] = &[">&-", "<&-"];

/// Python's recursion limit.
pub const RECURSION_LIMIT: i64 = 1000;

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Decomposition {
    pub ok: bool,
    pub heads: Vec<String>,
    pub commands: Vec<String>,
    pub reads: Vec<String>,
    pub writes: Vec<String>,
    pub reason: String,
}

pub struct ShellParser {
    parser: Parser,
}

impl Default for ShellParser {
    fn default() -> Self {
        Self::new()
    }
}

/// Node kinds whose spans may legitimately contain a raw newline; a newline
/// anywhere else inside a `command` node is a statement terminator the GLR
/// parser fused into one node (G-4).
const MULTILINE_LEGAL: &[&str] = &[
    "string",
    "raw_string",
    "ansi_c_string",
    "translated_string",
    "command_substitution",
    "process_substitution",
    "arithmetic_expansion",
    "heredoc_body",
    "heredoc_redirect",
];

/// Splitting at every bare newline strictly reduces the newline count, so a
/// fragment can never re-fuse; this bound only trips on a logic bug.
const MAX_FUSION_SPLIT_DEPTH: i64 = 64;

const FUSION_REASON: &str = "ambiguous shell (bare newline inside command — parser statement fusion)";

fn reject(reason: impl Into<String>) -> Decomposition {
    Decomposition { ok: false, reason: reason.into(), ..Default::default() }
}

/// `node.text.decode("utf-8", "replace")`.
fn text(node: &Node, src: &[u8]) -> String {
    String::from_utf8_lossy(&src[node.start_byte()..node.end_byte()]).into_owned()
}

fn children<'t>(node: &Node<'t>) -> Vec<Node<'t>> {
    let mut c = node.walk();
    node.children(&mut c).collect()
}

/// Every node of the tree with its depth (the root is 0), in pre-order.
fn preorder<'t>(root: Node<'t>) -> Vec<(Node<'t>, i64)> {
    let mut out = Vec::new();
    let mut c = root.walk();
    let mut depth = 0i64;
    loop {
        out.push((c.node(), depth));
        if c.goto_first_child() {
            depth += 1;
            continue;
        }
        loop {
            if c.goto_next_sibling() {
                break;
            }
            if !c.goto_parent() {
                return out;
            }
            depth -= 1;
        }
    }
}

/// `_literal_text`.
fn literal_text(node: &Node, src: &[u8]) -> Option<String> {
    match node.kind() {
        "word" => Some(text(node, src)),
        "raw_string" => {
            let t: Vec<char> = text(node, src).chars().collect();
            // Python's [1:-1] on the decoded text.
            if t.len() >= 2 {
                Some(t[1..t.len() - 1].iter().collect())
            } else {
                Some(String::new())
            }
        }
        "string" => {
            let parts: Vec<Node> = children(node).into_iter().filter(|c| c.kind() != "\"").collect();
            if parts.iter().all(|c| c.kind() == "string_content") {
                Some(parts.iter().map(|c| text(c, src)).collect::<String>())
            } else {
                None
            }
        }
        _ => None,
    }
}

enum RedirectClass {
    Read(String),
    Write(String),
    Passthrough,
    Reject(String),
}

/// `_classify_file_redirect`.
fn classify_file_redirect(node: &Node, src: &[u8]) -> RedirectClass {
    let mut operator: Option<&'static str> = None;
    let mut destination: Option<Node> = None;
    for child in children(node) {
        if child.kind() == "file_descriptor" {
            continue;
        }
        if operator.is_none() {
            operator = Some(child.kind());
            continue;
        }
        if destination.is_some() {
            return RedirectClass::Reject(format!(
                "ambiguous shell (a redirect with more than one target {})",
                repr(&text(node, src))
            ));
        }
        destination = Some(child);
    }
    if operator.is_some_and(|o| FD_CLOSE_OPS.contains(&o)) && destination.is_none() {
        return RedirectClass::Passthrough;
    }
    let (operator, destination) = match (operator, destination) {
        (Some(o), Some(d)) => (o, d),
        _ => return RedirectClass::Reject(format!("unrecognized shell redirection ({})", repr(&text(node, src)))),
    };
    if destination.kind() == "number" {
        if operator == ">&" || operator == "<&" {
            return RedirectClass::Passthrough;
        }
        return RedirectClass::Reject(format!("unrecognized shell redirection ({})", repr(&text(node, src))));
    }
    let path = match literal_text(&destination, src) {
        Some(p) => p,
        None => {
            return RedirectClass::Reject(format!(
                "non-literal redirect target ({})",
                repr(&text(&destination, src))
            ))
        }
    };
    if WRITE_REDIRECT_OPS.contains(&operator) {
        return RedirectClass::Write(path);
    }
    if READ_REDIRECT_OPS.contains(&operator) {
        return RedirectClass::Read(path);
    }
    RedirectClass::Reject(format!("unrecognized shell redirection operator ({})", repr(operator)))
}

/// `_bare_newline_offsets` of one `command` node, and the most frames its
/// helpers put on the stack above the frame of `_bare_newline_offsets`
/// itself: `collect` one frame per level below the command, down to and
/// including each multiline-legal node, and the `any(...)` generator.
fn bare_newline_offsets(node: Node, src: &[u8]) -> (Vec<usize>, i64) {
    let mut protected: Vec<(usize, usize)> = Vec::new();
    // collect(node) runs one frame above _bare_newline_offsets.
    let mut deepest = 1i64;
    let mut stack: Vec<(Node, i64)> = vec![(node, 1)];
    while let Some((m, f)) = stack.pop() {
        deepest = deepest.max(f);
        if MULTILINE_LEGAL.contains(&m.kind()) {
            protected.push((m.start_byte(), m.end_byte()));
            continue;
        }
        let kids = children(&m);
        for k in kids.into_iter().rev() {
            stack.push((k, f + 1));
        }
    }
    // Python appends protected spans in pre-order; only membership matters.
    let mut offsets = Vec::new();
    for i in node.start_byte()..node.end_byte() {
        let b = src[i];
        if b != b'\n' && b != b'\r' {
            continue;
        }
        // The any(...) generator runs one frame above.
        deepest = deepest.max(1);
        if protected.iter().any(|&(a, z)| a <= i && i < z) {
            continue;
        }
        if i > 0 && src[i - 1] == b'\\' {
            continue;
        }
        offsets.push(i);
    }
    (offsets, deepest)
}

/// `_rewrite_fused_newlines`.
fn rewrite_fused_newlines(src: &[u8], offsets: &[usize], comment_ends: &HashSet<usize>) -> Vec<u8> {
    let cut: HashSet<usize> = offsets.iter().copied().collect();
    let mut out: Vec<u8> = Vec::with_capacity(src.len());
    for (i, &b) in src.iter().enumerate() {
        if cut.contains(&i) && !comment_ends.contains(&i) {
            let prev = out.iter().rev().find(|&&c| c != 0x20 && c != 0x09).copied();
            out.push(match prev {
                Some(b';') | Some(b'&') | Some(b'|') => 0x20,
                _ => b';',
            });
        } else {
            out.push(b);
        }
    }
    out
}

impl ShellParser {
    pub fn new() -> Self {
        let language = tree_sitter_bash::LANGUAGE;
        let mut parser = Parser::new();
        parser.set_language(&language.into()).expect("failed to load bash grammar");
        ShellParser { parser }
    }

    fn parse(&mut self, src: &[u8]) -> Option<Tree> {
        self.parser.parse(src, None)
    }

    /// `decompose_command` with no stack bound, for the conformance client.
    pub fn decompose(&mut self, command: &str) -> Decomposition {
        match self.decompose_py(command, None) {
            Ok(d) => d,
            Err(e) => reject(e.msg),
        }
    }

    /// `decompose_command(command)` as the oracle runs it with `frames`
    /// Python frames on the stack at the call (the caller's included), or
    /// with no stack bound when `frames` is None. Raises what the oracle
    /// raises: UnicodeEncodeError for a lone surrogate, RecursionError for
    /// a tree deeper than the stack allows.
    pub fn decompose_py(&mut self, command: &str, frames: Option<i64>) -> Result<Decomposition, PyErr> {
        let src = encode_utf8_strict(command)?;
        self.decompose_bytes(src, frames, 0)
    }

    fn decompose_bytes(&mut self, src: Vec<u8>, frames: Option<i64>, pass: i64) -> Result<Decomposition, PyErr> {
        let tree = match self.parse(&src) {
            Some(t) => t,
            None => return Ok(reject("malformed shell (parse error)")),
        };
        let root = tree.root_node();
        if root.has_error() {
            return Ok(reject("malformed shell (parse error)"));
        }
        // _all_fused_newline_offsets: walk(node) runs at
        // frames + 4 + pass + depth (decompose_command, _decompose for each
        // pass, _all_fused_newline_offsets, walk from the root).
        let nodes = preorder(root);
        let mut fused: BTreeSet<usize> = BTreeSet::new();
        let mut deepest = 0i64;
        for (n, d) in &nodes {
            let at = 4 + pass + d;
            deepest = deepest.max(at);
            if n.kind() == "command" {
                let (offs, extra) = bare_newline_offsets(*n, &src);
                // _bare_newline_offsets runs one frame above walk.
                deepest = deepest.max(at + 1 + extra);
                fused.extend(offs);
            }
        }
        if let Some(f) = frames {
            if f + deepest > RECURSION_LIMIT {
                return Err(PyErr::recursion());
            }
        }
        if !fused.is_empty() {
            if pass >= MAX_FUSION_SPLIT_DEPTH {
                return Err(PyErr::new(
                    "AssertionError",
                    "fusion-repair recursion exceeded; a rewrite re-fused, which should be impossible (each pass \
                     replaces newlines with ';')",
                ));
            }
            let comment_ends: HashSet<usize> =
                nodes.iter().filter(|(n, _)| n.kind() == "comment").map(|(n, _)| n.end_byte()).collect();
            let offsets: Vec<usize> = fused.into_iter().collect();
            let rewritten = rewrite_fused_newlines(&src, &offsets, &comment_ends);
            if rewritten == src {
                return Ok(reject(FUSION_REASON));
            }
            let clean = self.parse(&rewritten).is_some_and(|t| !t.root_node().has_error());
            if !clean {
                return Ok(reject(FUSION_REASON));
            }
            // _decompose(rewritten.decode("utf-8", "replace"), _depth + 1)
            let next = String::from_utf8_lossy(&rewritten).into_owned().into_bytes();
            return self.decompose_bytes(next, frames, pass + 1);
        }
        Ok(visit_all(root, &src))
    }

    /// Probes whether the grammar is usable (`shell_decompose.parser_available`).
    pub fn available(&mut self) -> bool {
        self.decompose("a && b").ok
    }
}

/// `visit(root)` of `_decompose`, as a loop: a pre-order walk that stops at
/// the first refusal, classifies each file redirect without walking below
/// it, and records each literal command head before walking its children.
fn visit_all(root: Node, src: &[u8]) -> Decomposition {
    let mut heads = Vec::new();
    let mut commands = Vec::new();
    let mut reads = Vec::new();
    let mut writes = Vec::new();
    let mut stack: Vec<Node> = vec![root];
    while let Some(node) = stack.pop() {
        if node.is_missing() {
            return reject("malformed shell (missing token)");
        }
        match node.kind() {
            "file_redirect" => {
                match classify_file_redirect(&node, src) {
                    RedirectClass::Reject(r) => return reject(r),
                    RedirectClass::Read(p) => reads.push(p),
                    RedirectClass::Write(p) => writes.push(p),
                    RedirectClass::Passthrough => {}
                }
                continue;
            }
            "command" => {
                let name = match node.child_by_field_name("name") {
                    Some(n) => n,
                    None => return reject("command with no resolvable head"),
                };
                let kinds: Vec<&str> = children(&name).iter().map(|c| c.kind()).collect();
                if kinds != ["word"] {
                    return reject(format!("non-literal command head ({})", repr(&text(&name, src))));
                }
                heads.push(text(&name, src));
                commands.push(text(&node, src));
            }
            _ => {}
        }
        for k in children(&node).into_iter().rev() {
            stack.push(k);
        }
    }
    if heads.is_empty() {
        return reject("no command heads found");
    }
    Decomposition { ok: true, heads, commands, reads, writes, reason: String::new() }
}

/// The tree height below the root of a command's parse, for callers that
/// size their own recursion (the frames `walk` needs are 4 more than this
/// at the first pass).
pub fn tree_height(parser: &mut ShellParser, command: &str) -> Option<i64> {
    let src = command.as_bytes();
    let tree = parser.parse(src)?;
    Some(preorder(tree.root_node()).iter().map(|(_, d)| *d).max().unwrap_or(0))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_redirect_with_more_than_one_target_is_refused() {
        let mut p = ShellParser::new();
        for c in ["ls | FOO=1 </work/i rm x", "rm </dev/null -rf /work", "cat <a b", "echo hi >out more"] {
            let d = p.decompose(c);
            assert!(!d.ok, "{c:?} should be refused");
            assert!(d.reason.starts_with("ambiguous shell (a redirect with more than one target"), "{}", d.reason);
        }
        for c in ["<x cat y", "FOO=1 </x rm y z", "echo hi > out", "ls 2>&1 >/w/o"] {
            assert!(p.decompose(c).ok, "{c:?} should be accepted");
        }
        assert_eq!(p.decompose("cat <a b").reason, "ambiguous shell (a redirect with more than one target '<a b')");
        assert_eq!(p.decompose("$X a").reason, "non-literal command head ('$X')");
        assert_eq!(p.decompose("ls > \"$O\"").reason, "non-literal redirect target ('\"$O\"')");
    }

    #[test]
    fn fusion_across_newline_is_split_into_all_heads() {
        let mut p = ShellParser::new();
        let d = p.decompose("a1 | b1 | c1\nd1 | e1 > f.txt");
        assert!(d.ok, "{}", d.reason);
        assert_eq!(d.heads, vec!["a1", "b1", "c1", "d1", "e1"]);
        assert_eq!(d.writes, vec!["f.txt"]);
    }

    #[test]
    fn multiline_script_recovers_every_head_not_just_the_survivors() {
        let mut p = ShellParser::new();
        let d = p.decompose("echo one && grep p f | grep q | head\necho two | ls");
        assert!(d.ok, "{}", d.reason);
        for h in ["echo", "grep", "head", "ls"] {
            assert!(d.heads.iter().any(|x| x == h), "missing head {h:?} in {:?}", d.heads);
        }
    }

    #[test]
    fn quoted_newline_continuation_heredoc_and_substitution() {
        let mut p = ShellParser::new();
        assert_eq!(p.decompose("echo \"line one\nline two\"").heads, vec!["echo"]);
        assert_eq!(p.decompose("echo foo \\\nbar").heads, vec!["echo"]);
        assert_eq!(p.decompose("cat <<EOF\nhello\nworld\nEOF").heads, vec!["cat"]);
        assert!(p.decompose("x=$(echo a\necho b)").heads.iter().any(|x| x == "echo"));
        let d = p.decompose("ok1 | ok2 | ok3\n$MYSTERY arg");
        assert!(!d.ok && d.reason.contains("non-literal"), "{}", d.reason);
    }

    #[test]
    fn rewrite_does_not_create_double_semicolon() {
        let none = HashSet::new();
        assert_eq!(rewrite_fused_newlines(b"a;\nb", &[2], &none), b"a; b");
        assert_eq!(rewrite_fused_newlines(b"a\n\nb", &[1, 2], &none), b"a; b");
        assert_eq!(rewrite_fused_newlines(b"a\nb", &[1], &none), b"a;b");
    }

    #[test]
    fn fusion_across_a_comment_does_not_hide_the_post_comment_head() {
        let mut p = ShellParser::new();
        let d = p.decompose("grep a b | grep c | head\n# restore\nsed y | tail\ncmd z > out");
        assert!(d.ok, "{}", d.reason);
        for h in ["grep", "head", "sed", "tail", "cmd"] {
            assert!(d.heads.iter().any(|x| x == h), "{h} hidden by comment");
        }
        assert!(d.writes.iter().any(|w| w == "out"));
    }

    #[test]
    fn deep_trees_raise_where_python_would_and_never_overflow() {
        let mut p = ShellParser::new();
        let deep = vec!["a"; 20_000].join(" && ");
        assert_eq!(p.decompose_py(&deep, Some(10)).unwrap_err().kind, "RecursionError");
        assert!(p.decompose_py(&deep, None).unwrap().ok);
        let shallow = vec!["a"; 100].join(" && ");
        assert!(p.decompose_py(&shallow, Some(10)).unwrap().ok);
    }

    #[test]
    fn a_lone_surrogate_raises_as_the_utf8_codec_does() {
        let mut p = ShellParser::new();
        let s = format!("ls {}", crate::gate::py::text::surrogate_char(0xd800));
        let e = p.decompose_py(&s, Some(10)).unwrap_err();
        assert_eq!(e.kind, "UnicodeEncodeError");
    }
}
