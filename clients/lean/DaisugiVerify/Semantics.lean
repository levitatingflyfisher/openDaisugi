/-
Direct ports of the small pure functions in verify.py that decide shell-head
allowlisting, file-scope glob matching, the metacharacter gate, the simple-
command head classifier, and strict-mode resolution. These are unit-tested
against `clients/fixtures/semantics.json` (see `TestSemantics.lean`).

Style note: everything here works on `List Char` internally and converts at
the `String` boundary via `.toList` / `String.ofList` — Lean 4.33's `String`
is byte-array-backed with a `Slice`-returning `take`/`drop`/`get` API, so
`List Char` (stable, well-understood, no surprises) is the safer internal
representation for character-position logic.
-/
import DaisugiVerify.Basic

namespace DaisugiVerify

/-- Python `str.split(sep)` — keeps empty strings, unlike `split()` with no
args. Needed because `_head_allowed` splits on `/` WITHOUT dropping empty
segments (a leading `/` produces a leading `""` segment that must align). -/
def splitChar (sep : Char) (s : String) : List String :=
  s.splitOn sep.toString

/-- Python `str.split()` with no arguments: split on runs of any whitespace,
dropping empty results. Used by `extractHead`. -/
def splitWhitespace (s : String) : List String :=
  (s.splitToList (fun c => c == ' ' || c == '\t' || c == '\n' || c == '\r'))
    |>.filter (fun t => !t.isEmpty)

/-- `[*?\[]` — does the allowlist entry carry any glob metacharacter? -/
def hasGlobChars (s : String) : Bool :=
  s.toList.any (fun c => c == '*' || c == '?' || c == '[')

/-- Port of `_head_allowed`. Literal entries need exact equality; glob
entries are matched segment-by-segment (both split on `/`, segment COUNTS
must be equal — this left-anchors the match, unlike the file-scope
matcher). -/
def headAllowed (head : String) (allowlist : List String) : Bool :=
  allowlist.any fun pat =>
    if head == pat then true
    else if !hasGlobChars pat then false
    else
      let headSegs := splitChar '/' head
      let patSegs := splitChar '/' pat
      headSegs.length == patSegs.length &&
        (headSegs.zip patSegs).all (fun (h, p) => globMatch p.toList h.toList)

/-- Leading-slash classification for POSIX `normpath`: 0 = relative, 1 =
single leading slash (collapses any run), 2 = the "exactly two leading
slashes" POSIX quirk (three-or-more collapses to a single slash). -/
def leadingSlashCount (cs : List Char) : Nat :=
  match cs with
  | '/' :: '/' :: '/' :: _ => 1
  | '/' :: '/' :: _ => 2
  | '/' :: _ => 1
  | _ => 0

/-- Port of `posixpath.normpath` (POSIX semantics: collapse `.` components,
resolve `..` against non-`..`/non-root predecessors, collapse redundant
slashes, and preserve the POSIX quirk that exactly two leading slashes are
significant while three-or-more collapse to one). -/
def normpath (path : String) : String :=
  if path.isEmpty then "."
  else
    let cs := path.toList
    let leadSlashes := leadingSlashCount cs
    let comps := splitChar '/' path
    let newComps := comps.foldl (init := ([] : List String)) fun acc comp =>
      if comp == "" || comp == "." then acc
      else if comp != ".." then acc ++ [comp]
      else if (leadSlashes == 0 && acc.isEmpty) || acc.getLast? == some ".." then acc ++ [".."]
      else if acc.isEmpty then acc ++ [".."]
      else acc.dropLast
    let joined := String.intercalate "/" newComps
    let withSlash := if leadSlashes > 0 then String.ofList (List.replicate leadSlashes '/') ++ joined else joined
    if withSlash.isEmpty then "." else withSlash

/-- Segment-list matcher for `_match_glob`'s general (non-`/**`-suffix)
case: `patSegs` may contain a literal `"**"` segment meaning "zero or more
path segments"; every other pattern segment is matched against exactly one
path segment via `globMatch` (case-sensitive `fnmatch`-equivalent —
`*`/`?`/`[...]`, no cross-segment wildcarding). The whole pattern must
consume the whole path (left- AND right-anchored — this is the fix for
F-1: `PurePosixPath.match`'s right-anchoring let a relative pattern like
`["out.txt"]` admit `/etc/cron.d/out.txt`, a scope escape).

Total by construction, no `partial def`: every recursive call strictly
decreases `patSegs.length + pathSegs.length` (each `"**"` branch drops one
element from exactly one of the two lists; the plain-segment branch drops
one from both; every other shape is a base case with no recursion). -/
def matchSegs (patSegs pathSegs : List String) : Bool :=
  match patSegs, pathSegs with
  | [], [] => true
  | [], _ :: _ => false
  | "**" :: prest, [] => matchSegs prest []
  | "**" :: prest, p :: prest' => matchSegs prest (p :: prest') || matchSegs ("**" :: prest) prest'
  | _ :: _, [] => false
  | pat :: prest, p :: prest' => globMatch pat.toList p.toList && matchSegs prest prest'
termination_by patSegs.length + pathSegs.length

/-- Port of `_match_glob` (native left-anchored, `/`-aware matcher —
replaces the old `PurePosixPath.match`-based port; fixes F-1 (right-
anchoring let a relative pattern admit an absolute path outside its
scope) and F-2 (`PurePosixPath.match`'s mid-pattern `**` behavior was
Python-version-dependent — this matcher always treats `**` as recursive,
on every Python) per ADJUDICATIONS.md, oracle-side fix landed
2026-08-21). A pattern ending in `/**` gets a dedicated prefix-anchored
handler (mirrors the oracle's own special case — `**`-as-whole-glob and
`./**`-as-any-relative-path need root/relative special cases that don't
reduce to "consume one path segment per pattern segment"). Otherwise:
split both pattern and path on `/` WITHOUT dropping empty segments (an
absolute path's leading `/` produces a leading `""` segment that must
still line up on both sides — this is exactly why `splitChar`, Python
`str.split`, is used here rather than `pathlibSegments`, which used to
filter those out), then hand the whole thing to `matchSegs`. -/
def matchGlobOne (normalizedPath glob : String) : Bool :=
  let gcs := glob.toList
  if gcs.length >= 3 && gcs.drop (gcs.length - 3) == ['/', '*', '*'] then
    let rawCs := gcs.take (gcs.length - 3)
    if rawCs.isEmpty then
      -- "/**" — the root: any absolute path.
      match normalizedPath.toList with
      | '/' :: _ => true
      | _ => false
    else
      let pfx := normpath (String.ofList rawCs)
      if pfx == "." then
        -- "./**" — any relative path (never an absolute one, and not
        -- exactly ".." or a path starting with a "../" escape upward).
        let ncs := normalizedPath.toList
        (match ncs with | '/' :: _ => false | _ => true) &&
          normalizedPath != ".." &&
          !(ncs.take 3 == ['.', '.', '/'])
      else
        -- norm == pfx, or norm starts with pfx ++ "/".
        let pcs := pfx.toList
        let ncs := normalizedPath.toList
        normalizedPath == pfx ||
          (ncs.take pcs.length == pcs &&
            match ncs.drop pcs.length with
            | '/' :: _ => true
            | _ => false)
  else
    matchSegs (splitChar '/' glob) (splitChar '/' normalizedPath)

/-- Port of `_path_matches_any`: normalize once, try every glob. -/
def pathMatchesAny (path : String) (globs : List String) : Bool :=
  let normalized := normpath path
  globs.any (matchGlobOne normalized)

/-- `[;|&`<>\n\r]|\$\(` — the metachar gate, scanned on the RAW (unstripped)
command string. -/
partial def hasMetachar (s : String) : Bool :=
  let rec go : List Char → Bool
    | [] => false
    | '$' :: '(' :: _ => true
    | c :: rest =>
      (c == ';' || c == '|' || c == '&' || c == '`' || c == '<' || c == '>' ||
        c == '\n' || c == '\r') || go rest
  go s.toList

/-- `^[A-Za-z_][A-Za-z0-9_]*=` — verify's OWN (looser-than-decompose's)
assignment-prefix pattern, used only by `extractHead`. Digit-leading names
are NOT skipped here (contrast `decompose`'s tree-sitter-derived rule,
which does skip them — the two are deliberately different, see
PORTING-NOTES.md). -/
def looksLikeEnvAssign (tok : String) : Bool :=
  match tok.toList with
  | [] => false
  | c :: rest =>
    (c.isAlpha || c == '_') &&
      match rest.span (fun ch => ch.isAlpha || ch.isDigit || ch == '_') with
      | (_, '=' :: _) => true
      | _ => false

/-- Port of `_extract_shell_head`. `stripped` must already be `.strip()`-ed
by the caller, mirroring the oracle's call sites. -/
def extractHead (stripped : String) : Option String :=
  match stripped.toList with
  | [] => none
  | '#' :: _ => none
  | _ =>
    let rec go : List String → Option String
      | [] => none
      | tok :: rest => if looksLikeEnvAssign tok then go rest else some tok
    go (splitWhitespace stripped)

/-- Port of `resolve_strict`: explicit bool wins; `none` defaults to `true`
for high/physical stakes. -/
def resolveStrict (strict : Option Bool) (stakes : String) : Bool :=
  match strict with
  | some b => b
  | none => stakes == "high" || stakes == "physical"

end DaisugiVerify
