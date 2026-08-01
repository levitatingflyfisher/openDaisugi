/-
Small shared utilities: char classification and glob matching (fnmatch-case
equivalent) used by both the semantics port (Semantics.lean) and the
decompose subset parser (ShellDecompose.lean). No shell-grammar recursion
lives here — just primitives.
-/

namespace DaisugiVerify

/-- Whitespace that separates shell tokens but is not itself a separator
token (unlike newline, which IS a list-level separator). -/
def isBlank (c : Char) : Bool :=
  c == ' ' || c == '\t'

/-- The whitespace set Python's `str.strip()` (no args) trims: space, tab,
newline, CR, form feed, vertical tab. Avoids `String.trim`/`trimAscii`,
whose exact behavior and return type (`String` vs `String.Slice`) have
been in flux across recent Lean versions — this is self-contained and
`List Char`-based like the rest of this file. -/
def isPyStripChar (c : Char) : Bool :=
  c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\x0c' || c == '\x0b'

def pyStrip (s : String) : String :=
  String.ofList ((s.toList.dropWhile isPyStripChar).reverse.dropWhile isPyStripChar).reverse

/-- `[A-Za-z0-9_]` — the character class tree-sitter-bash's
`variable_assignment` NAME token accepts (confirmed empirically against the
oracle: `1FOO=1 cmd` skips as an assignment; `FOO-BAR=1 cmd` does not, so
`-`/`.` are excluded). -/
def isNameChar (c : Char) : Bool :=
  c.isAlpha || c.isDigit || c == '_'

/-- A string of only ASCII digits, non-empty — tree-sitter's `number` node
shape used for the `>&`/`<&` fd-duplication carve-out. -/
def isAllDigits (s : String) : Bool :=
  !s.isEmpty && s.toList.all Char.isDigit

/-- Scans a `[...]` character-class body (the corpus never uses ranges —
verified empirically, no `-` appears inside brackets in any real envelope —
so this supports plain enumeration and a leading `!`/`^` negation only, no
range expansion). Returns `(members, negate, rest-after-])` or `none` if
the class is unterminated (malformed — caller falls back to literal `[`). -/
partial def scanClass (rest : List Char) : Option (List Char × Bool × List Char) :=
  match rest with
  | '!' :: rest' => go rest' [] |>.map (fun (m, r) => (m, true, r))
  | '^' :: rest' => go rest' [] |>.map (fun (m, r) => (m, true, r))
  | _ => go rest [] |>.map (fun (m, r) => (m, false, r))
where
  go (r : List Char) (acc : List Char) : Option (List Char × List Char) :=
    match r with
    | ']' :: r' => some (acc, r')
    | c :: r' => go r' (c :: acc)
    | [] => none

/-- Case-sensitive glob match with `*`, `?`, `[...]`. Mirrors Python's
`fnmatch.fnmatchcase` restricted to the wildcard forms `_head_allowed`'s
callers ever construct (verified against the corpus: no ranges, no
brace/extglob forms in any real allowlist entry). `**` behaves exactly
like `*` here — this function does path-segment-free char matching; the
distinct `**`-as-recursive-vs-single-segment question belongs to the glob
CALLERS (`_head_allowed` splits on `/` and requires equal segment counts
regardless of `**`; the file-scope matcher has its own `/**` handler) —
neither delegates recursive-descent semantics to this function. -/
partial def globMatch (pat : List Char) (s : List Char) : Bool :=
  match pat, s with
  | [], [] => true
  | [], _ :: _ => false
  | '*' :: prest, _ =>
    globMatch prest s || (match s with
      | [] => false
      | _ :: srest => globMatch pat srest)
  | '?' :: prest, _ :: srest => globMatch prest srest
  | '?' :: _, [] => false
  | '[' :: prest, c :: srest =>
    match scanClass prest with
    | none => c == '[' && globMatch prest srest  -- unterminated class: '[' is literal
    | some (members, negate, prest') =>
      let hit := members.contains c
      if hit != negate then globMatch prest' srest else false
  | pc :: prest, c :: srest => pc == c && globMatch prest srest
  | _ :: _, [] => false

end DaisugiVerify
