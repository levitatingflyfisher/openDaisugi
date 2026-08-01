//! Port of `opendaisugi/shell_decompose.py`: fail-closed compound-shell
//! decomposition via tree-sitter-bash. See the module docstring there and
//! `clients/PORTING-NOTES.md` for the exact fail-closed rules this must
//! reproduce byte-for-byte in *behavior* (not wording — `reason` is
//! informative on the wire).

use tree_sitter::{Node, Parser};

const WRITE_REDIRECT_OPS: &[&str] = &[">", ">>", "&>", "&>>", ">|", ">&"];
const READ_REDIRECT_OPS: &[&str] = &["<", "<&"];
const FD_CLOSE_OPS: &[&str] = &[">&-", "<&-"];

#[derive(Debug, Clone, Default)]
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

impl ShellParser {
    pub fn new() -> Self {
        let language = tree_sitter_bash::LANGUAGE;
        let mut parser = Parser::new();
        parser.set_language(&language.into()).expect("failed to load bash grammar");
        ShellParser { parser }
    }

    pub fn decompose(&mut self, command: &str) -> Decomposition {
        self.decompose_depth(command, 0)
    }

    /// `_decompose` — the recursive core. `depth` only ever increases across a
    /// fusion-repair re-parse (see `MAX_FUSION_SPLIT_DEPTH`); the ordinary walk
    /// never recurses back into this method.
    fn decompose_depth(&mut self, command: &str, depth: usize) -> Decomposition {
        let src = command.as_bytes();
        let tree = match self.parser.parse(src, None) {
            Some(t) => t,
            None => return reject("malformed shell (parser failure)"),
        };
        let root = tree.root_node();
        if root.has_error() {
            return reject("malformed shell (parse error)");
        }

        // G-4 repair: tree-sitter-bash 0.25.1 can fuse a newline statement
        // boundary into a single `command` node, so a later head would
        // execute unchecked (fail-OPEN). Recover the true decomposition by
        // rewriting each fused bare newline to an explicit `;` — the
        // unambiguous separator tree-sitter will not fuse — and re-parsing
        // the WHOLE command, so compound context (if/then/else, loops) is
        // preserved. If the rewrite does not parse cleanly (`;` is invalid
        // right after `then`/`do`/`else`), the fusion was genuinely ambiguous
        // to resolve locally: fail closed, exactly as before.
        let fused = all_fused_newline_offsets(root, src);
        if !fused.is_empty() {
            if depth >= MAX_FUSION_SPLIT_DEPTH {
                panic!(
                    "fusion-repair recursion exceeded; a rewrite re-fused, which \
                     should be impossible (each pass replaces newlines with ';')"
                );
            }
            let rewritten = rewrite_fused_newlines(src, &fused, &comment_end_offsets(root));
            if rewritten == src {
                // Every fused newline terminates a comment; none can be
                // rewritten to `;` without burying the next statement in the
                // comment. Can't resolve locally — fail closed, don't hide a head.
                return reject(
                    "ambiguous shell (bare newline inside command — parser statement fusion)",
                );
            }
            let clean = self
                .parser
                .parse(&rewritten, None)
                .is_some_and(|t| !t.root_node().has_error());
            if !clean {
                return reject(
                    "ambiguous shell (bare newline inside command — parser statement fusion)",
                );
            }
            let rewritten_command = String::from_utf8_lossy(&rewritten).into_owned();
            return self.decompose_depth(&rewritten_command, depth + 1);
        }

        let mut heads = Vec::new();
        let mut commands = Vec::new();
        let mut reads = Vec::new();
        let mut writes = Vec::new();
        let mut reason: Option<String> = None;

        visit(root, src, &mut heads, &mut commands, &mut reads, &mut writes, &mut reason);

        if let Some(r) = reason {
            return Decomposition { ok: false, reason: r, ..Default::default() };
        }
        if heads.is_empty() {
            return reject("no command heads found");
        }
        Decomposition { ok: true, heads, commands, reads, writes, reason: String::new() }
    }

    /// Probes whether the grammar is actually usable (mirrors
    /// `shell_decompose.parser_available`, always true once construction
    /// succeeded, since Rust has no "extra not installed" failure mode).
    pub fn available(&mut self) -> bool {
        self.decompose("a && b").ok
    }
}

fn reject(reason: &str) -> Decomposition {
    Decomposition { ok: false, reason: reason.to_string(), ..Default::default() }
}

fn text_lossy(node: &Node, src: &[u8]) -> String {
    let bytes = &src[node.start_byte()..node.end_byte()];
    String::from_utf8_lossy(bytes).into_owned()
}

/// `_literal_text` — the literal string a destination/word node denotes, or
/// `None` if it is not fully known before execution.
fn literal_text(node: &Node, src: &[u8]) -> Option<String> {
    match node.kind() {
        "word" => Some(text_lossy(node, src)),
        "raw_string" => {
            let t = text_lossy(node, src);
            let chars: Vec<char> = t.chars().collect();
            if chars.len() >= 2 {
                Some(chars[1..chars.len() - 1].iter().collect())
            } else {
                Some(String::new())
            }
        }
        "string" => {
            let mut cursor = node.walk();
            let children: Vec<Node> = node.children(&mut cursor).collect();
            let parts: Vec<Node> = children.into_iter().filter(|c| c.kind() != "\"").collect();
            if parts.iter().all(|c| c.kind() == "string_content") {
                Some(parts.iter().map(|c| text_lossy(c, src)).collect::<Vec<_>>().join(""))
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
    let mut cursor = node.walk();
    for child in node.children(&mut cursor) {
        if child.kind() == "file_descriptor" {
            continue;
        }
        if operator.is_none() {
            operator = Some(child.kind());
            continue;
        }
        destination = Some(child);
        break;
    }

    if operator.is_some_and(|o| FD_CLOSE_OPS.contains(&o)) && destination.is_none() {
        return RedirectClass::Passthrough;
    }
    let (operator, destination) = match (operator, destination) {
        (Some(o), Some(d)) => (o, d),
        _ => return RedirectClass::Reject(format!("unrecognized shell redirection ({:?})", text_lossy(node, src))),
    };
    if destination.kind() == "number" {
        if operator == ">&" || operator == "<&" {
            return RedirectClass::Passthrough;
        }
        return RedirectClass::Reject(format!("unrecognized shell redirection ({:?})", text_lossy(node, src)));
    }
    let path = match literal_text(&destination, src) {
        Some(p) => p,
        None => {
            return RedirectClass::Reject(format!(
                "non-literal redirect target ({:?})",
                text_lossy(&destination, src)
            ));
        }
    };
    if WRITE_REDIRECT_OPS.contains(&operator) {
        return RedirectClass::Write(path);
    }
    if READ_REDIRECT_OPS.contains(&operator) {
        return RedirectClass::Read(path);
    }
    RedirectClass::Reject(format!("unrecognized shell redirection operator ({operator:?})"))
}

/// The pre-order walk. `file_redirect` nodes classify and never recurse
/// beneath themselves; `command` nodes record a literal head then FALL
/// THROUGH to keep walking children (substitutions inside arguments surface
/// their own inner heads); every other node just recurses.
/// Node kinds whose spans may legitimately contain a raw newline; a newline
/// anywhere else inside a `command` node is a tree-sitter statement-fusion
/// artifact (`c1\nd1` parsed as command `c1` arg `d1`) — a statement
/// terminator the GLR parser fused into one node, repaired in
/// `decompose_depth` (G-4).
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

fn collect_protected(node: Node, out: &mut Vec<(usize, usize)>) {
    if MULTILINE_LEGAL.contains(&node.kind()) {
        out.push((node.start_byte(), node.end_byte()));
        return;
    }
    let mut cursor = node.walk();
    for child in node.children(&mut cursor) {
        collect_protected(child, out);
    }
}

/// Byte offsets of statement-terminator newlines fused into a `command` node
/// — the exact points where the fused statements must be cut apart. Every raw
/// `\n`/`\r` offset inside the command's own span that is not within a
/// multiline-legal child and not a `\` line-continuation. Empty means no
/// fusion. (Port of `_bare_newline_offsets`.)
fn bare_newline_offsets(node: Node, src: &[u8]) -> Vec<usize> {
    let mut protected: Vec<(usize, usize)> = Vec::new();
    collect_protected(node, &mut protected);
    let mut offsets = Vec::new();
    for i in node.start_byte()..node.end_byte() {
        let b = src[i];
        if b != b'\n' && b != b'\r' {
            continue;
        }
        if protected.iter().any(|&(a, z)| a <= i && i < z) {
            continue;
        }
        if i > 0 && src[i - 1] == b'\\' {
            continue;
        }
        offsets.push(i);
    }
    offsets
}

/// Union of fused-newline offsets across every `command` node in the whole
/// tree, sorted. (Port of `_all_fused_newline_offsets`.)
fn all_fused_newline_offsets(root: Node, src: &[u8]) -> Vec<usize> {
    let mut offsets: std::collections::BTreeSet<usize> = std::collections::BTreeSet::new();

    fn walk(node: Node, src: &[u8], offsets: &mut std::collections::BTreeSet<usize>) {
        if node.kind() == "command" {
            offsets.extend(bare_newline_offsets(node, src));
        }
        let mut cursor = node.walk();
        for child in node.children(&mut cursor) {
            walk(child, src, offsets);
        }
    }

    walk(root, src, &mut offsets);
    offsets.into_iter().collect()
}

/// Replace each fused newline with `;` — the separator tree-sitter won't
/// fuse — but with a space where a `;` would abut an existing separator (a
/// trailing `;` or a blank line would otherwise yield `;;`, a `case`-only
/// token that is a syntax error anywhere else). (Port of
/// `_rewrite_fused_newlines`.)
/// End offsets of `comment` nodes — the newline that terminates each `#`
/// comment. A fused newline here must NOT become `;`: a comment runs to the
/// newline, so rewriting it would pull the next statement into the comment and
/// hide its head (a fail-OPEN that `has_error` cannot catch, since
/// `head;# note;sed` is valid shell). Keep such newlines verbatim.
fn comment_end_offsets(root: Node) -> std::collections::HashSet<usize> {
    let mut ends = std::collections::HashSet::new();
    fn walk(node: Node, ends: &mut std::collections::HashSet<usize>) {
        if node.kind() == "comment" {
            ends.insert(node.end_byte());
        }
        let mut cursor = node.walk();
        for child in node.children(&mut cursor) {
            walk(child, ends);
        }
    }
    walk(root, &mut ends);
    ends
}

fn rewrite_fused_newlines(
    src: &[u8],
    offsets: &[usize],
    comment_ends: &std::collections::HashSet<usize>,
) -> Vec<u8> {
    let cut: std::collections::HashSet<usize> = offsets.iter().copied().collect();
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

/// Splitting at every bare newline strictly reduces the newline count, so a
/// fragment can never re-fuse — this bound only trips on a logic bug, loudly.
const MAX_FUSION_SPLIT_DEPTH: usize = 64;

#[allow(clippy::too_many_arguments)]
fn visit(
    node: Node,
    src: &[u8],
    heads: &mut Vec<String>,
    commands: &mut Vec<String>,
    reads: &mut Vec<String>,
    writes: &mut Vec<String>,
    reason: &mut Option<String>,
) {
    if reason.is_some() {
        return;
    }
    if node.is_missing() {
        *reason = Some("malformed shell (missing token)".to_string());
        return;
    }
    match node.kind() {
        "file_redirect" => {
            match classify_file_redirect(&node, src) {
                RedirectClass::Reject(r) => *reason = Some(r),
                RedirectClass::Read(p) => reads.push(p),
                RedirectClass::Write(p) => writes.push(p),
                RedirectClass::Passthrough => {}
            }
            return;
        }
        "command" => {
            // No fusion check here: by the time the walk runs,
            // `decompose_depth` has already repaired (or rejected) every
            // fused bare newline in the whole tree, so every `command` node
            // reaching this point is genuinely clean.
            let name = match node.child_by_field_name("name") {
                Some(n) => n,
                None => {
                    *reason = Some("command with no resolvable head".to_string());
                    return;
                }
            };
            let mut cursor = name.walk();
            let kinds: Vec<&str> = name.children(&mut cursor).map(|c| c.kind()).collect();
            if kinds.len() != 1 || kinds[0] != "word" {
                *reason = Some(format!("non-literal command head ({:?})", text_lossy(&name, src)));
                return;
            }
            heads.push(text_lossy(&name, src));
            commands.push(text_lossy(&node, src));
            // Fall through: arguments may hold substitutions.
        }
        _ => {}
    }
    let mut cursor = node.walk();
    for child in node.children(&mut cursor) {
        visit(child, src, heads, commands, reads, writes, reason);
        if reason.is_some() {
            return;
        }
    }
}

/// G-4 (found by the Go + Lean client ports): tree-sitter statement fusion.
/// Mirrors `tests/test_shell_decompose_fusion.py` — see that module's
/// docstring for the full story. tree-sitter-bash 0.25.1's GLR parser
/// sometimes fuses a newline-separated statement boundary into a single
/// `command` node (`c1\nd1` parses as command `c1` with argument `d1`)
/// without setting `has_error`; left unrepaired, `d1` would execute without
/// ever facing the allowlist (fail-OPEN). `decompose_depth` repairs this by
/// rewriting each fused bare newline to `;` and re-parsing, recovering every
/// hidden head; a rewrite that doesn't parse cleanly still fails closed.
#[cfg(test)]
mod tests {
    use super::*;

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
    fn quoted_newline_argument_is_not_a_fusion() {
        let mut p = ShellParser::new();
        let d = p.decompose("echo \"line one\nline two\"");
        assert!(d.ok, "{}", d.reason);
        assert_eq!(d.heads, vec!["echo"]);
    }

    #[test]
    fn line_continuation_is_not_a_fusion() {
        let mut p = ShellParser::new();
        let d = p.decompose("echo foo \\\nbar");
        assert!(d.ok, "{}", d.reason);
        assert_eq!(d.heads, vec!["echo"]);
    }

    #[test]
    fn heredoc_body_newlines_are_not_a_fusion() {
        let mut p = ShellParser::new();
        let d = p.decompose("cat <<EOF\nhello\nworld\nEOF");
        assert!(d.ok, "{}", d.reason);
        assert_eq!(d.heads, vec!["cat"]);
    }

    #[test]
    fn command_substitution_spanning_lines_surfaces_inner_heads() {
        let mut p = ShellParser::new();
        let d = p.decompose("x=$(echo a\necho b)");
        assert!(d.ok, "{}", d.reason);
        assert!(d.heads.iter().any(|x| x == "echo"));
    }

    #[test]
    fn fused_fragment_with_nonliteral_head_still_fails_closed() {
        let mut p = ShellParser::new();
        let d = p.decompose("ok1 | ok2 | ok3\n$MYSTERY arg");
        assert!(!d.ok);
        assert!(d.reason.contains("non-literal"), "{}", d.reason);
    }

    #[test]
    fn rewrite_does_not_create_double_semicolon() {
        let none = std::collections::HashSet::new();
        // 'a;\nb' — offset 2 is the fused newline, preceded by ';'
        assert_eq!(rewrite_fused_newlines(b"a;\nb", &[2], &none), b"a; b");
        // 'a\n\nb' — blank line: two fused newlines in a row must not double up
        assert_eq!(rewrite_fused_newlines(b"a\n\nb", &[1, 2], &none), b"a; b");
        // ordinary case still gets a real separator
        assert_eq!(rewrite_fused_newlines(b"a\nb", &[1], &none), b"a;b");
    }

    #[test]
    fn fusion_across_a_comment_does_not_hide_the_post_comment_head() {
        // Rewriting the newline that TERMINATES a '# restore' comment to ';'
        // would bury the following statements in the comment (fail-OPEN).
        // Every post-comment head must survive.
        let mut p = ShellParser::new();
        let d = p.decompose("grep a b | grep c | head\n# restore\nsed y | tail\ncmd z > out");
        assert!(d.ok, "{}", d.reason);
        for h in ["grep", "head", "sed", "tail", "cmd"] {
            assert!(d.heads.iter().any(|x| x == h), "{h} hidden by comment");
        }
        assert!(d.writes.iter().any(|w| w == "out"));
    }

    #[test]
    fn compound_spanning_fusion_fails_closed_not_wrong() {
        // When tree-sitter fuses across an if/then/else boundary, rewriting
        // the bare newlines to ';' yields invalid shell ('then;'/'else;'), so
        // we fail CLOSED rather than emit a bogus 'else' head. Safe, not
        // silently wrong.
        let mut p = ShellParser::new();
        let d = p.decompose("if [ -d x ]; then\ngrep a b | c | d\ne > f\nelse\ng\nfi");
        if !d.ok {
            assert!(
                d.reason.contains("fusion") || d.reason.contains("malformed"),
                "{}",
                d.reason
            );
        } else {
            assert!(!d.heads.iter().any(|h| h == "else"));
            assert!(!d.heads.iter().any(|h| h == "fi"));
        }
    }
}
