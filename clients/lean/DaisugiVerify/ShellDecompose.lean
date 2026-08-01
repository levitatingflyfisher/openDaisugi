/-
The decompose subset parser — a fail-closed hand-rolled recursive-descent
parser over a bash SUBSET, standing in for shell_decompose.py's real
tree-sitter-bash grammar (see PORTING-NOTES.md and clients/lean/README.md
for the exact scope cut). Every decision here was checked empirically
against the oracle (`.venv/bin/python3`, NOT system python — see the
README's Python-version note) before being encoded; where the oracle
accepts more than this subset, that is a deliberate, documented, SAFE
scope cut (a false reject, never a false accept).

Supported: `; & && || | |&` sequencing/pipes, the `!` pipeline-negation
prefix, subshells `( … )`, `$( … )` command substitution and `` `…` ``
backticks (both recursive, anywhere including inside double quotes and
glued to other text), `${…}` parameter expansion and `$(( … ))` arithmetic
(as non-literal expansions — a substitution inside either fails closed),
single/double-quoted words (double-quote literalness tracked per
tree-sitter's `string_content`-only rule), backslash escapes (uniformly:
protects exactly the next character, raw text kept, matching tree-sitter's
un-interpreted token text), `#` comments at a token boundary, leading
env-assignment tokens (`[A-Za-z0-9_]+=`, digit-leading names included) —
INCLUDING values with substitutions, whose heads are deferred to after the
command head to match the oracle's emission order (`FOO=$(a) cmd` →
`[cmd, a]`), literal file redirects with the fd-dup/number-shaped quirk,
`<<<` herestrings, heredocs `<<`/`<<-` (body consumed at the list level; an
unquoted body carrying a substitution fails closed), and the reserved-word
compounds `if`/`while`/`until`/`for`/`select` (each keyword-delimited span
decomposed recursively; the `for`/`select` header is parsed strictly).

Deliberately NOT supported (each a documented scope cut, always a safe
reject): process substitution (`<(`, `>(`), `case`/`function`/brace groups,
C-style `for ((…))`, and a heredoc that is not the last token of a piped
command (`cmd <<EOF 2>&1 | tail` — a tree-sitter parse error the oracle
raises and `mvdan/sh` rejects too, so this fails closed to conform). See
`docs/client-diversity.md` for the campaign that grew this subset from
8,882 to 11,097 / 11,450 behind a fail-open-proof gate.
-/
import DaisugiVerify.Basic

namespace DaisugiVerify

structure DecompState where
  heads  : Array String := #[]
  reads  : Array String := #[]
  writes : Array String := #[]
  -- Full source text of each simple command, 1:1 with `heads` (NOT part of
  -- the decompose wire comparison — `commands` is informative-only there —
  -- but genuinely needed by Verify.lean to recurse `parse_interpreter` over
  -- each decomposed piece exactly like the oracle's `_verify_simple_command`
  -- does for every entry in `decomp.commands`).
  commands : Array String := #[]
  -- Heredoc delimiters declared on the current line but whose bodies (which live
  -- after the next newline) have not been consumed yet: (delim, isDash, quoted).
  -- Drained by `parseList` when it crosses the newline; a non-empty `pending` at
  -- end of input is an unterminated heredoc (fail closed).
  pending : Array (String × Bool × Bool) := #[]
  -- Set when a token (redirect or argument) is parsed AFTER a heredoc operator on
  -- the same command. tree-sitter-bash (the oracle) mis-parses a heredoc that is
  -- NOT the last token of a PIPED command (`cmd <<EOF 2>&1 | tail`) as a parse
  -- error, and mvdan/sh rejects it too; a subset client must fail closed at the
  -- pipe to match. Reset per pipeline segment (see `parsePipeline`).
  hdTrail : Bool := false

inductive WordKind where
  | word (s : String)   -- pure unquoted literal: head- and destination-eligible
  | sqstr (s : String)  -- pure single-quoted literal: destination-eligible only
  | dqstr (s : String)  -- pure double-quoted literal (no expansions): destination-eligible only
  | mixed                -- concatenation / contains a substitution / non-literal dquote / bare $var
  deriving BEq

def WordKind.literalText? : WordKind → Option String
  | .word s => some s
  | .sqstr s => some s
  | .dqstr s => some s
  | .mixed => none

def WordKind.isPlainWord : WordKind → Option String
  | .word s => some s
  | _ => none

/-- Characters that always end an unquoted run (never consumed literally
without an escaping backslash): blank, list/pipe/redirect/paren
punctuation, newline, quote starts, backtick, `#`. `$` is handled
separately (a bare `$` doesn't stop the run; `$(`/`${` do). -/
def isRunStop (c : Char) : Bool :=
  isBlank c || c == ';' || c == '&' || c == '|' || c == '<' || c == '>' ||
    c == '(' || c == ')' || c == '\n' || c == '\'' || c == '"' || c == '`' || c == '#'

/-- Bash reserved words and brace/test-bracket keywords: in real bash
grammar these are NOT `command` nodes when they appear in head position
(the oracle recurses into their bodies, surfacing only the commands
inside — `for`/`while`/`if`/`case`/brace-group/`function`/`[[` constructs
are all out of this subset's scope, see the module doc). Rejecting the
whole command when one of these would-be-head tokens appears in head
position is the fail-closed alternative to mis-parsing them as literal
program names. -/
def isReservedWord (s : String) : Bool :=
  ["if", "then", "elif", "else", "fi", "for", "while", "until", "do", "done",
   "case", "esac", "select", "function", "in", "{", "}", "]]", "!"].contains s

/-- `[` and `[[` are tree-sitter's `test_command` grammar rule, and
`export`/`declare`/`typeset`/`readonly`/`local`/`unset` are its
`declaration_command` rule — NEITHER is a `command` node. Empirically
verified: `[ "$(x)" = y ] && echo hi` surfaces `x`'s head but never `[`
itself, a substitution-free `[ -f a -a -d b ]` alone contributes zero
heads, `export PATH="/x"` alone contributes zero heads, and
`export FOO=$(git rev-parse HEAD)` still surfaces `git`. So these are
"headless": scan the rest of the tokens for embedded substitutions
(exactly like any argument run) without ever pushing a head or a
`commands` entry. -/
def isHeadlessKeyword (s : String) : Bool :=
  s == "[" || s == "[[" ||
    ["export", "declare", "typeset", "readonly", "local", "unset"].contains s

/-- Redirect operator kinds. `write`/`read` carry the classification for a
LITERAL destination; `fdWriteAmp`/`fdReadAmp` are `>&`/`<&` (literal
destination -> write/read same as `write`/`read`; NUMBER-shaped
destination -> pathless fd-dup, handled by the caller); `fdClose` (`>&-`,
`<&-`) consumes no destination at all; `herestring` (`<<<`) consumes one
token but records nothing; `heredoc` (`<<`, `<<-`) is recognized only so
it can be rejected with a clear reason. -/
inductive RedirOp where
  | write | read | fdWriteAmp | fdReadAmp | fdClose | herestring | heredoc | heredocDash

/-- Longest-match redirect operator recognition at `pos`. Returns
`(consumedLen, op)`. A bare `&` only starts a redirect when immediately
followed by `>` (`&>`/`&>>`) — a lone `&` is the background/list
separator, left to `atCommandBoundary`. -/
def matchRedirectOp (cs : Array Char) (pos : Nat) : Option (Nat × RedirOp) :=
  let peekC (i : Nat) : Option Char := cs[pos + i]?
  match peekC 0 with
  | some '>' =>
    match peekC 1 with
    | some '>' => some (2, .write)                                    -- >>
    | some '&' =>
      if peekC 2 == some '-' then some (3, .fdClose) else some (2, .fdWriteAmp)  -- >&- / >&
    | some '|' => some (2, .write)                                    -- >|
    | _ => some (1, .write)                                           -- >
  | some '<' =>
    match peekC 1, peekC 2 with
    | some '<', some '<' => some (3, .herestring)                     -- <<<
    | some '<', some '-' => some (3, .heredocDash)                    -- <<-
    | some '<', _ => some (2, .heredoc)                               -- <<
    | some '&', some '-' => some (3, .fdClose)                        -- <&-
    | some '&', _ => some (2, .fdReadAmp)                             -- <&
    | _, _ => some (1, .read)                                         -- <
  | some '&' =>
    match peekC 1, peekC 2 with
    | some '>', some '>' => some (3, .write)                          -- &>>
    | some '>', _ => some (2, .write)                                 -- &>
    | _, _ => none
  | _ => none

def recordRedirect (op : RedirOp) (text : String) (st : DecompState) : DecompState :=
  match op with
  | .write | .fdWriteAmp => { st with writes := st.writes.push text }
  | .read | .fdReadAmp => { st with reads := st.reads.push text }
  | _ => st

/-- `$((` was just consumed; `pos` points at the arithmetic expression. Skips
to the matching `))`, tracking `(`/`)` nesting. Arithmetic is an expansion (a
number), never a command, so nothing is recorded — but a command substitution
(`$(`) or backtick inside CAN carry a head the oracle would surface, so those
fail closed (the safe direction; nested arithmetic is rare). Returns the
position just past the closing `))`, or `none` to reject. -/
partial def skipArith (cs : Array Char) (pos : Nat) (depth : Nat) : Option Nat :=
  if pos >= cs.size then none
  else
    let c := cs[pos]!
    if c == '`' then none
    else if c == '$' && pos + 1 < cs.size && cs[pos+1]! == '(' then none
    else if c == '(' then skipArith cs (pos + 1) (depth + 1)
    else if c == ')' then
      if depth > 0 then skipArith cs (pos + 1) (depth - 1)
      else if pos + 1 < cs.size && cs[pos+1]! == ')' then some (pos + 2)
      else none
    else skipArith cs (pos + 1) depth

/-- `${` was just consumed; `pos` points at the parameter-expansion body. Skips
to the matching `}`, tracking `{`/`}` nesting. A parameter expansion is not a
command; but a command substitution (`$(`) or backtick inside a word-op
(`${x:-$(cmd)}`) CAN carry a head the oracle would surface, so those fail closed
(the safe direction). Returns the position just past the closing `}`, or `none`. -/
partial def skipParamExp (cs : Array Char) (pos : Nat) (depth : Nat) : Option Nat :=
  if pos >= cs.size then none
  else
    let c := cs[pos]!
    if c == '`' then none
    else if c == '$' && pos + 1 < cs.size && cs[pos+1]! == '(' then none
    else if c == '{' then skipParamExp cs (pos + 1) (depth + 1)
    else if c == '}' then
      if depth > 0 then skipParamExp cs (pos + 1) (depth - 1) else some (pos + 1)
    else skipParamExp cs (pos + 1) depth

/-! ### Heredoc body consumption (positional, no parser recursion)

A heredoc (`<<EOF` / `<<-EOF`) redirects stdin *data*, not a file, and its body
lives on the lines AFTER the operator's own line — so `parseRedirect` cannot
consume it in place. Instead it records a pending delimiter, and the list level
consumes the body when it crosses the next newline (see `parseList`). The helpers
below read the delimiter and the body purely positionally. -/

/-- Skip spaces/tabs only (heredoc delimiter may follow `<<` with optional blanks). -/
partial def skipHSpace (cs : Array Char) (pos : Nat) : Nat :=
  if pos < cs.size && (cs[pos]! == ' ' || cs[pos]! == '\t') then skipHSpace cs (pos + 1) else pos

/-- Read to a closing quote `q`, returning the interior text and the position
just past the quote. `none` if unterminated. -/
partial def readQuotedTo (cs : Array Char) (pos : Nat) (q : Char) (acc : List Char) :
    Option (Nat × String) :=
  if pos >= cs.size then none
  else if cs[pos]! == q then some (pos + 1, String.ofList acc.reverse)
  else readQuotedTo cs (pos + 1) q (cs[pos]! :: acc)

/-- Read a plain (unquoted) heredoc delimiter word: letters/digits/`_`/`-`/`.`
until a blank or shell metacharacter. -/
partial def readPlainDelim (cs : Array Char) (pos : Nat) (acc : List Char) : Nat × String :=
  if pos >= cs.size then (pos, String.ofList acc.reverse)
  else
    let c := cs[pos]!
    if c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' ||
        c == '<' || c == '>' || c == '(' || c == ')' || c == '\'' || c == '"' || c == '`' then
      (pos, String.ofList acc.reverse)
    else readPlainDelim cs (pos + 1) (c :: acc)

/-- Read the heredoc delimiter after `<<`/`<<-`. Returns
`(posAfter, delimText, quoted)`. A `'…'`/`"…"`-quoted delimiter means the body
is literal (no expansion); an unquoted one may carry expansions. `none` on an
unreadable (empty or unterminated-quote) delimiter — fail closed. -/
def readHeredocDelim (cs : Array Char) (pos0 : Nat) : Option (Nat × String × Bool) :=
  let pos := skipHSpace cs pos0
  if pos >= cs.size then none
  else
    let c := cs[pos]!
    if c == '\'' then (readQuotedTo cs (pos + 1) '\'' []).map (fun (p, s) => (p, s, true))
    else if c == '"' then (readQuotedTo cs (pos + 1) '"' []).map (fun (p, s) => (p, s, true))
    else
      let (p, s) := readPlainDelim cs pos []
      if s.isEmpty then none else some (p, s, false)

/-- Position of the next `\n` at or after `pos` (or `cs.size`). -/
partial def lineEndAt (cs : Array Char) (pos : Nat) : Nat :=
  if pos >= cs.size || cs[pos]! == '\n' then pos else lineEndAt cs (pos + 1)

/-- Skip leading tabs (for `<<-` delimiter/body matching). -/
partial def skipTabs (cs : Array Char) (pos : Nat) : Nat :=
  if pos < cs.size && cs[pos]! == '\t' then skipTabs cs (pos + 1) else pos

/-- Does the byte range `[a, b)` contain a `$(` command substitution or a
backtick? Used to fail closed on an UNQUOTED heredoc body whose expansions the
oracle would recurse into (dropping their heads is the one unsafe outcome). -/
partial def rangeHasSubst (cs : Array Char) (a b : Nat) : Bool :=
  if a >= b then false
  else if cs[a]! == '`' then true
  else if cs[a]! == '$' && a + 1 < cs.size && cs[a+1]! == '(' then true
  else rangeHasSubst cs (a + 1) b

/-- Consume one heredoc body starting at line `pos`: read lines until one equals
`delim` (leading tabs stripped when `isDash`). Returns the position just past the
terminator line. Fails closed on an unterminated body, or (when `!quoted`) a body
line carrying a substitution. -/
partial def consumeOneHeredoc (cs : Array Char) (pos : Nat) (delim : String)
    (isDash quoted : Bool) : Option Nat :=
  if pos > cs.size then none
  else
    let lineEnd := lineEndAt cs pos
    let contentStart := if isDash then skipTabs cs pos else pos
    let content := String.ofList ((cs.extract contentStart lineEnd).toList)
    if content == delim then
      some (if lineEnd < cs.size then lineEnd + 1 else lineEnd)
    else if lineEnd >= cs.size then none  -- ran out of input before the delimiter
    else if (!quoted) && rangeHasSubst cs pos lineEnd then none  -- unquoted body subst: fail closed
    else consumeOneHeredoc cs (lineEnd + 1) delim isDash quoted

/-- Consume all pending heredoc bodies (in declaration order) from `pos`. -/
partial def consumeHeredocs (cs : Array Char) (pos : Nat)
    (pend : Array (String × Bool × Bool)) (i : Nat) : Option Nat :=
  if i >= pend.size then some pos
  else
    let (delim, isDash, quoted) := pend[i]!
    match consumeOneHeredoc cs pos delim isDash quoted with
    | none => none
    | some pos' => consumeHeredocs cs pos' pend (i + 1)

/-! ### Reserved-word compound recognition (peeking)

`for`/`while`/`until`/`if`/`case`/`select` open compound commands; their
`do`/`then`/`fi`/`done`/`esac`/`in`/`elif`/`else` keywords delimit the
command-bearing spans inside. The parser recognises an opener at command
position and drives the structure; it treats a delimiter keyword as a place to
STOP a list (so the compound parser regains control). Both need to peek a bare
lowercase-letter keyword token WITHOUT consuming or side-effecting. -/

/-- Is `c` (or end-of-input) a token boundary after a bare keyword? -/
def kwBoundary : Option Char → Bool
  | none => true
  | some c => isBlank c || c == ';' || c == '\n' || c == ')' || c == '&' ||
      c == '|' || c == '<' || c == '>'

/-- Maximal run of lowercase letters from `pos`. -/
partial def readLetters (cs : Array Char) (pos : Nat) (acc : List Char) : Nat × List Char :=
  if pos < cs.size && cs[pos]! ≥ 'a' && cs[pos]! ≤ 'z' then
    readLetters cs (pos + 1) (cs[pos]! :: acc)
  else (pos, acc)

/-- Peek a bare lowercase-letter keyword token at `pos` (a run of letters ending
at a word boundary), or `""` if `pos` is not such a token. -/
def peekKw (cs : Array Char) (pos : Nat) : String :=
  let (p, acc) := readLetters cs pos []
  if p == pos || !kwBoundary cs[p]? then "" else String.ofList acc.reverse

def isOpenerKw (s : String) : Bool :=
  ["for", "while", "until", "if", "case", "select"].contains s

/-- Consume a `[A-Za-z0-9_]` run. -/
partial def readNameRun (cs : Array Char) (pos : Nat) : Nat :=
  if pos < cs.size && isNameChar cs[pos]! then readNameRun cs (pos + 1) else pos

/-- A `for`/`select` loop variable: `[A-Za-z_][A-Za-z0-9_]*`. Returns the
position just past it, or `none` (a digit-led or absent name, e.g. a C-style
`for ((…))`, fails closed). -/
def readName (cs : Array Char) (pos : Nat) : Option Nat :=
  if pos < cs.size && isNameChar cs[pos]! && !('0' ≤ cs[pos]! && cs[pos]! ≤ '9') then
    some (readNameRun cs (pos + 1))
  else none

/-- Keywords that terminate a command list inside a compound (so a list parser
stops and hands the keyword back to the compound driver). -/
def isTerminatorKw (s : String) : Bool :=
  ["then", "elif", "else", "fi", "do", "done", "esac", "in"].contains s

/-- Append `b`'s contributions onto `a` (order-preserving for heads/commands;
reads/writes are sorted at the wire so their order is immaterial). Used to hold a
command's leading-assignment substitution heads aside and splice them in AFTER
the command head, matching the oracle's emission order (`FOO=$(a) cmd` →
`[cmd, a]`, head first). -/
def mergeStates (a b : DecompState) : DecompState :=
  { a with heads := a.heads ++ b.heads, reads := a.reads ++ b.reads,
           writes := a.writes ++ b.writes, commands := a.commands ++ b.commands }

mutual

/-- Scans one maximal unquoted run starting at `pos` (which must not
already be a stop char). Backslash universally protects exactly the next
character (raw pair kept, no reinterpretation) — this is how `a\;b`
becomes one literal word and how `\"`/`\$`/`` \` `` survive inside a
double-quoted piece too (`scanDoubleQuoted` delegates escape handling
here isn't reused directly, but follows the identical rule). A bare `$`
(not `$(`/`${`) doesn't stop the run but downgrades the piece to
non-literal (`sawDollar`). Returns `none` only on a trailing lone
backslash at end of input (mirrors the oracle's parse-error there). -/
partial def scanRun (cs : Array Char) (pos : Nat) (acc : List Char) (sawDollar : Bool) :
    Option (Nat × List Char × Bool) :=
  if pos >= cs.size then some (pos, acc, sawDollar)
  else
    let c := cs[pos]!
    if c == '\\' then
      if pos + 1 >= cs.size then none  -- trailing lone backslash: reject
      else scanRun cs (pos + 2) (cs[pos+1]! :: c :: acc) sawDollar
    else if c == '$' then
      if pos + 1 < cs.size && (cs[pos+1]! == '(' || cs[pos+1]! == '{') then
        some (pos, acc, sawDollar)  -- stop: caller handles $( / ${
      else scanRun cs (pos + 1) (c :: acc) true
    else if isRunStop c then some (pos, acc, sawDollar)
    else scanRun cs (pos + 1) (c :: acc) sawDollar

partial def scanSingleQuoted (cs : Array Char) (pos : Nat) (acc : List Char) :
    Option (Nat × List Char) :=
  if pos >= cs.size then none
  else
    let c := cs[pos]!
    if c == '\'' then some (pos + 1, acc) else scanSingleQuoted cs (pos + 1) (c :: acc)

/-- Double-quoted content: `"` closes; `\` protects exactly the next char
(kept raw, matches the oracle's un-interpreted `string_content` text —
verified empirically: `"a\$b"`, `"a\"b"`, `"a\nb"` are ALL literal with
the backslash preserved). An unescaped `$(` recurses (merging into `st`
immediately); an unescaped `` ` ``/`${` rejects the WHOLE command; any of
those also mark the piece non-literal (`literal := false`). -/
partial def scanDoubleQuoted (cs : Array Char) (pos : Nat) (acc : List Char) (literal hadSubst : Bool)
    (st : DecompState) : Option (Nat × List Char × Bool × Bool × DecompState) :=
  if pos >= cs.size then none
  else
    let c := cs[pos]!
    if c == '"' then some (pos + 1, acc, literal, hadSubst, st)
    else if c == '\\' then
      if pos + 1 >= cs.size then none
      else scanDoubleQuoted cs (pos + 2) (cs[pos+1]! :: c :: acc) literal hadSubst st
    else if c == '`' then
      match scanBacktick cs (pos + 1) [] st with
      | none => none
      | some (pos', st') => scanDoubleQuoted cs pos' acc false true st'
    else if c == '$' && pos + 1 < cs.size && cs[pos+1]! == '{' then
      match skipParamExp cs (pos + 2) 0 with
      | none => none
      | some pos' => scanDoubleQuoted cs pos' acc false hadSubst st
    else if c == '$' && pos + 1 < cs.size && cs[pos+1]! == '(' then
      if pos + 2 < cs.size && cs[pos+2]! == '(' then
        match skipArith cs (pos + 3) 0 with
        | none => none
        | some pos' => scanDoubleQuoted cs pos' acc false hadSubst st
      else
        match scanSubstitution cs (pos + 2) st with
        | none => none
        | some (pos', st') => scanDoubleQuoted cs pos' acc false true st'
    else if c == '$' then
      -- bare $VAR (or a lone trailing $): expansion, not literal, but nothing to recurse into
      scanDoubleQuoted cs (pos + 1) (c :: acc) false hadSubst st
    else scanDoubleQuoted cs (pos + 1) (c :: acc) literal hadSubst st

/-- `$(` was just consumed; `pos` points just past it. Parses the body as
a nested command LIST (recursing through the whole grammar), then
consumes the matching `)`. Merges the inner heads/reads/writes into `st`
immediately (this IS the ordering the oracle produces: an outer head is
recorded before its own arguments'/assignments' substitution heads, and
substitutions merge in left-to-right scan order — see the module doc). -/
partial def scanSubstitution (cs : Array Char) (pos : Nat) (st : DecompState) :
    Option (Nat × DecompState) :=
  match parseList cs pos true st with
  | none => none
  | some (pos', st') =>
    if pos' < cs.size && cs[pos']! == ')' then some (pos' + 1, st') else none

/-- Backtick command substitution. `pos` points just past the opening `` ` ``;
`acc` collects the body (reversed). Scans to the matching unescaped `` ` ``, then
recursively decomposes the body as a command list, merging inner heads into
`st` (the same treatment `$(…)` gets — the oracle recurses both). Any backslash
inside the body (escaped `` \` `` nesting, `\$`, `\\`) fails closed: nested
backticks are the one construct where a naive close-scan would drop heads, so
rejecting is the safe direction. Returns (posPastClose, st'). -/
partial def scanBacktick (cs : Array Char) (pos : Nat) (acc : List Char) (st : DecompState) :
    Option (Nat × DecompState) :=
  if pos >= cs.size then none            -- unterminated backtick
  else
    let c := cs[pos]!
    if c == '\\' then none               -- escaped/nested backtick territory: fail closed
    else if c == '`' then
      let body := (String.ofList acc.reverse).toList.toArray
      match parseList body 0 false st with
      | none => none
      | some (bpos, st') => if bpos == body.size then some (pos + 1, st') else none
    else scanBacktick cs (pos + 1) (c :: acc) st

/-- Scans one whole token (one or more glued pieces). `firstPiece = true`
only for the very first piece; any additional glued piece (no whitespace
between) downgrades the whole token to `mixed`, matching tree-sitter's
`concatenation` node. Returns `none` on any unsupported construct
(unterminated quote, backtick, `${`, `$((`, process substitution, `#`
mid-token). -/
partial def scanWord (cs : Array Char) (pos : Nat) (kind : WordKind) (firstPiece : Bool)
    (st : DecompState) : Option (Nat × WordKind × DecompState) :=
  if pos >= cs.size then some (pos, kind, st)
  else
    let c := cs[pos]!
    if isRunStop c && c != '\'' && c != '"' && c != '`' && c != '#' then
      some (pos, kind, st)  -- blank / list / pipe / redirect / paren / newline: token ends
    else if c == '#' then
      if firstPiece then some (pos, kind, st)  -- caller handles comment-start
      else none  -- '#' mid-token: the oracle treats this as a parse error
    else if c == '`' then
      match scanBacktick cs (pos + 1) [] st with
      | none => none
      | some (pos', st') => scanWord cs pos' WordKind.mixed false st'
    else if c == '$' && pos + 1 < cs.size && cs[pos+1]! == '{' then
      match skipParamExp cs (pos + 2) 0 with
      | none => none
      | some pos' => scanWord cs pos' WordKind.mixed false st
    else if (c == '<' || c == '>') && pos + 1 < cs.size && cs[pos+1]! == '(' then
      some (pos, kind, st)  -- caller's structural dispatch rejects `<(`/`>(`
    else if c == '\'' then
      match scanSingleQuoted cs (pos + 1) [] with
      | none => none
      | some (pos', acc) =>
        let kind' := if firstPiece then WordKind.sqstr (String.ofList acc.reverse) else WordKind.mixed
        scanWord cs pos' kind' false st
    else if c == '"' then
      match scanDoubleQuoted cs (pos + 1) [] true false st with
      | none => none
      | some (pos', acc, lit, _hadSubst, st') =>
        let kind' :=
          if firstPiece && lit then WordKind.dqstr (String.ofList acc.reverse) else WordKind.mixed
        scanWord cs pos' kind' false st'
    else if c == '$' && pos + 1 < cs.size && cs[pos+1]! == '(' then
      if pos + 2 < cs.size && cs[pos+2]! == '(' then
        match skipArith cs (pos + 3) 0 with
        | none => none
        | some pos' => scanWord cs pos' WordKind.mixed false st
      else
        match scanSubstitution cs (pos + 2) st with
        | none => none
        | some (pos', st') => scanWord cs pos' WordKind.mixed false st'
    else
      match scanRun cs pos [] false with
      | none => none
      | some (pos', acc, sawDollar) =>
        if pos' == pos then some (pos, kind, st)  -- zero-width: nothing more to consume here
        else
          let kind' :=
            if firstPiece && !sawDollar then WordKind.word (String.ofList acc.reverse) else WordKind.mixed
          scanWord cs pos' kind' false st

/-- Skips leading blanks (space/tab only — newline is a separator token,
handled by the caller) AND backslash-newline line continuations (`\`
immediately followed by `\n` is whitespace-equivalent between tokens —
without this, a continued `&&` \ line starts its next token with a
literal `\n` glued on, producing a mangled head). -/
partial def skipBlanks (cs : Array Char) (pos : Nat) : Nat :=
  if pos < cs.size && isBlank cs[pos]! then skipBlanks cs (pos + 1)
  else if pos + 1 < cs.size && cs[pos]! == '\\' && cs[pos+1]! == '\n' then skipBlanks cs (pos + 2)
  else pos

/-- Peels leading assignment-shaped tokens (`[A-Za-z0-9_]+=…`, an unquoted
prefix immediately followed by `=`) off a simple command, accumulating any
substitution heads in their VALUES into `deferred` — a state held aside from the
main one. The oracle emits a command's head BEFORE its leading assignments'
value substitutions (`FOO=$(a) cmd` → `[cmd, a]`), so the caller splices
`deferred` in right after pushing the head (see `parseSimpleCommand`). Each
value is scanned in a FRESH state so its heads land in `deferred`, never
prematurely in the main head list. Returns the position just after the last
assignment token and the accumulated `deferred`. -/
partial def skipAssignments (cs : Array Char) (pos : Nat) (deferred : DecompState) :
    Option (Nat × DecompState) :=
  let p := skipBlanks cs pos
  if p >= cs.size then some (p, deferred)
  else
    match assignmentNameLen cs p 0 with
    | none => some (p, deferred)  -- not assignment-shaped: caller treats this as the head
    | some _ =>
      -- scan the whole token (name=value) in a fresh state; its value substitution
      -- heads accumulate into `deferred`, to be spliced after the command head.
      match scanWord cs p WordKind.mixed true {} with
      | none => none
      | some (pos', _kind, sub) => skipAssignments cs pos' (mergeStates deferred sub)

/-- Length of a `[A-Za-z0-9_]+` run immediately followed by `=`, or
`none` if this position isn't assignment-shaped. Pure lookahead — doesn't
consume. -/
partial def assignmentNameLen (cs : Array Char) (pos : Nat) (n : Nat) : Option Nat :=
  if pos < cs.size && isNameChar cs[pos]! then assignmentNameLen cs (pos + 1) (n + 1)
  else if n > 0 && pos < cs.size && cs[pos]! == '=' then some n else none

/-- One `simple_command`: leading assignments, then (optionally) a head,
then arguments/redirects until a delimiter. `inSubst` says whether `)`
ends this context (we're inside a `$(...)`/subshell). -/
partial def parseSimpleCommand (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  match skipAssignments cs pos {} with
  | none => none
  | some (pos1, deferred) =>
    let pos2 := skipBlanks cs pos1
    -- assignment-only command (no head): the deferred value substitutions ARE the heads.
    if atCommandBoundary cs pos2 inSubst then some (pos2, mergeStates st deferred)
    else if isCommentStart cs pos2 then some (skipToNewline cs pos2, mergeStates st deferred)
    else if isProcSubstStart cs pos2 then none
    else if cs[pos2]! == '(' then none  -- bare subshell start mid-command: unsupported here
    else if cs[pos2]! == ')' && !inSubst then none  -- stray close paren
    else
      -- the head (scanned against the MAIN state; a plain-word head carries no substitution)
      match scanWord cs pos2 WordKind.mixed true st with
      | none => none
      | some (pos3, kind, st2) =>
        match kind.isPlainWord with
        | none => none  -- non-literal command head
        | some headText =>
          if isTerminatorKw headText then some (pos2, mergeStates st deferred)  -- stop: hand the delimiter back to the list/compound driver
          else if isReservedWord headText then none  -- opener not intercepted, or {/}/!/]] : reject
          else if isHeadlessKeyword headText then
            parseArgsAndRedirects cs pos3 inSubst (mergeStates st2 deferred)  -- headless test_command: assignment substs still count
          else
          -- head first, THEN the leading assignments' value substitutions (oracle order)
          let st3 := mergeStates { st2 with heads := st2.heads.push headText } deferred
          match parseArgsAndRedirects cs pos3 inSubst st3 with
          | none => none
          | some (posFinal, stFinal) =>
            let text := String.ofList ((cs.extract pos2 posFinal).toList)
            some (posFinal, { stFinal with commands := stFinal.commands.push text })

partial def atCommandBoundary (cs : Array Char) (pos : Nat) (inSubst : Bool) : Bool :=
  if pos >= cs.size then true
  else
    let c := cs[pos]!
    c == ';' || c == '\n' || (c == ')' && inSubst) ||
      (c == '&' && !(pos + 1 < cs.size && cs[pos+1]! == '>')) ||
      (c == '|')

partial def isCommentStart (cs : Array Char) (pos : Nat) : Bool :=
  pos < cs.size && cs[pos]! == '#'

partial def skipToNewline (cs : Array Char) (pos : Nat) : Nat :=
  if pos >= cs.size || cs[pos]! == '\n' then pos else skipToNewline cs (pos + 1)

partial def isProcSubstStart (cs : Array Char) (pos : Nat) : Bool :=
  pos + 1 < cs.size && (cs[pos]! == '<' || cs[pos]! == '>') && cs[pos+1]! == '('

/-- Arguments and redirects after the head, until the next delimiter. -/
partial def parseArgsAndRedirects (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  let pos1 := skipBlanks cs pos
  if atCommandBoundary cs pos1 inSubst then some (pos1, st)
  else if isCommentStart cs pos1 then some (skipToNewline cs pos1, st)
  else if isProcSubstStart cs pos1 then none
  else if cs[pos1]! == '(' then none
  else if cs[pos1]! == ')' && !inSubst then none
  else
    -- A real token following an already-pending heredoc = content after the
    -- heredoc operator; flag it so `parsePipeline` can fail closed at a pipe.
    let st := if st.pending.isEmpty then st else { st with hdTrail := true }
    match matchRedirectOp cs pos1 with
    | some (opLen, kind) => parseRedirect cs pos1 opLen kind inSubst st
    | none =>
      match scanWord cs pos1 WordKind.mixed true st with
      | none => none
      | some (pos2, _kind, st') =>
        if pos2 == pos1 then none  -- couldn't make progress: malformed
        else parseArgsAndRedirects cs pos2 inSubst st'

partial def parseRedirect (cs : Array Char) (pos opLen : Nat) (op : RedirOp) (inSubst : Bool)
    (st : DecompState) : Option (Nat × DecompState) :=
  let pos1 := pos + opLen
  match op with
  | .fdClose => parseArgsAndRedirects cs pos1 inSubst st
  | .heredoc | .heredocDash =>
    match readHeredocDelim cs pos1 with
    | none => none
    | some (pos2, delim, quoted) =>
      if st.pending.size ≥ 1 then none  -- a second heredoc on one line: fail closed (first pass)
      else
        let isDash := match op with | .heredocDash => true | _ => false
        parseArgsAndRedirects cs pos2 inSubst
          { st with pending := st.pending.push (delim, isDash, quoted) }
  | .herestring =>
    let pos2 := skipBlanks cs pos1
    match scanWord cs pos2 WordKind.mixed true st with
    | none => none
    | some (pos3, _kind, st') =>
      if pos3 == pos2 then none else parseArgsAndRedirects cs pos3 inSubst st'
  | _ =>
    let pos2 := skipBlanks cs pos1
    match scanWord cs pos2 WordKind.mixed true st with
    | none => none
    | some (pos3, kind, st') =>
      if pos3 == pos2 then none  -- missing destination
      else
        match kind with
        | .word text =>
          if isAllDigits text then
            match op with
            | .fdWriteAmp | .fdReadAmp => parseArgsAndRedirects cs pos3 inSubst st'  -- fd dup: pathless
            | _ => none  -- number-shaped destination on a non-`&`-op redirect: reject
          else
            let st'' := recordRedirect op text st'
            parseArgsAndRedirects cs pos3 inSubst st''
        | .sqstr text | .dqstr text =>
          let st'' := recordRedirect op text st'
          parseArgsAndRedirects cs pos3 inSubst st''
        | .mixed => none  -- non-literal redirect target

/-- One command in command position: a subshell `( list )`, otherwise a simple
command. (Brace groups and reserved-word compounds are dispatched here too, in
later clauses.) A subshell reuses the `inSubst` paren-close mode: its inner list
is decomposed and merged into `st`, then any trailing redirects attach. -/
partial def parseCommand (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  let p := skipBlanks cs pos
  if p < cs.size && cs[p]! == '!' && (p + 1 >= cs.size || kwBoundary cs[p+1]?) then
    -- `!` pipeline negation: decompose the negated command, which MUST be a real
    -- (non-empty) command. `!` alone, `! |`, `$(!)` are shell errors — the
    -- recursion makes no progress past the blanks, so fail closed.
    let q := skipBlanks cs (p + 1)
    match parseCommand cs (p + 1) inSubst st with
    | none => none
    | some (pos', st') => if pos' > q then some (pos', st') else none
  else if p < cs.size && cs[p]! == '(' then
    match parseList cs (p + 1) true st with
    | none => none
    | some (pos', st') =>
      if pos' < cs.size && cs[pos']! == ')' then
        parseArgsAndRedirects cs (pos' + 1) inSubst st'
      else none
  else
    let kw := peekKw cs p
    if kw == "if" then parseIf cs (p + 2) inSubst st
    else if kw == "while" || kw == "until" then parseWhileUntil cs (p + kw.length) inSubst st
    else if kw == "for" || kw == "select" then parseForSelect cs (p + kw.length) inSubst st
    else parseSimpleCommand cs pos inSubst st

/-- Skip blanks and command separators (`;`, newline) — the glue between a
compound's keyword-delimited spans (`… ; do`, `… ; done`). -/
partial def skipSeps (cs : Array Char) (pos : Nat) : Nat :=
  let p := skipBlanks cs pos
  if p < cs.size && (cs[p]! == ';' || cs[p]! == '\n') then skipSeps cs (p + 1) else p

/-- Scan the WORDS of a `for NAME in WORDS` up to the `do` (surfacing any
substitution heads — `for f in $(ls)` still yields `ls` — but pushing no head
for the header words). WORDS end at `;`/newline, after which only `do` may
follow. Returns the position at `do`, or `none` if the header is malformed. -/
partial def scanForWords (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  let p := skipBlanks cs pos
  if peekKw cs p == "do" then some (p, st)
  else if p >= cs.size then none
  else if cs[p]! == ';' || cs[p]! == '\n' then
    let p2 := skipSeps cs p
    if peekKw cs p2 == "do" then some (p2, st) else none  -- only `do` may follow the words
  else
    match scanWord cs p WordKind.mixed true st with
    | none => none
    | some (p', _, st') => if p' == p then none else scanForWords cs p' inSubst st'

/-- Finish a `for`/`while`/`until`/`select`: at `do`, decompose the body list,
then require `done` (plus any trailing redirects). -/
partial def finishDoDone (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  if peekKw cs pos == "do" then
    match parseList cs (pos + 2) inSubst st with
    | none => none
    | some (pos1, st1) =>
      let p := skipSeps cs pos1
      if peekKw cs p == "done" then parseArgsAndRedirects cs (p + 4) inSubst st1 else none
  else none

/-- `for NAME [in WORDS]; do BODY; done` / `select NAME …`. Strict header: after
the loop variable only `in`, a separator, or `do` may follow — a misplaced word
(`for r $repos in; do`, a real shell syntax error) fails closed, matching the
oracle. The body is decomposed recursively. -/
partial def parseForSelect (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  let p0 := skipBlanks cs pos
  match readName cs p0 with
  | none => none  -- no valid loop variable (e.g. C-style `for ((…))`): fail closed
  | some p1 =>
    let p2 := skipBlanks cs p1
    if peekKw cs p2 == "in" then
      match scanForWords cs (p2 + 2) inSubst st with
      | none => none
      | some (p3, st1) => finishDoDone cs p3 inSubst st1
    else if peekKw cs p2 == "do" then finishDoDone cs p2 inSubst st
    else if p2 < cs.size && (cs[p2]! == ';' || cs[p2]! == '\n') then
      let p3 := skipSeps cs p2
      if peekKw cs p3 == "do" then finishDoDone cs p3 inSubst st else none
    else none  -- malformed for-header

partial def parseWhileUntil (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  match parseList cs pos inSubst st with
  | none => none
  | some (pos1, st1) =>
    let p := skipSeps cs pos1
    if peekKw cs p == "do" then
      match parseList cs (p + 2) inSubst st1 with
      | none => none
      | some (pos2, st2) =>
        let p2 := skipSeps cs pos2
        if peekKw cs p2 == "done" then parseArgsAndRedirects cs (p2 + 4) inSubst st2 else none
    else none

/-- `if COND; then BODY; [elif COND; then BODY;]* [else BODY;] fi`. Every span
between the keywords is a command list. -/
partial def parseIf (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  match parseList cs pos inSubst st with
  | none => none
  | some (pos1, st1) =>
    let p := skipSeps cs pos1
    if peekKw cs p == "then" then parseIfTail cs (p + 4) inSubst st1 else none

partial def parseIfTail (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  match parseList cs pos inSubst st with
  | none => none
  | some (pos1, st1) =>
    let p := skipSeps cs pos1
    let kw := peekKw cs p
    if kw == "fi" then parseArgsAndRedirects cs (p + 2) inSubst st1
    else if kw == "else" then
      match parseList cs (p + 4) inSubst st1 with
      | none => none
      | some (pos2, st2) =>
        let p2 := skipSeps cs pos2
        if peekKw cs p2 == "fi" then parseArgsAndRedirects cs (p2 + 2) inSubst st2 else none
    else if kw == "elif" then
      match parseList cs (p + 4) inSubst st1 with
      | none => none
      | some (pos2, st2) =>
        let p2 := skipSeps cs pos2
        if peekKw cs p2 == "then" then parseIfTail cs (p2 + 4) inSubst st2 else none
    else none

/-- `&&`/`||` chains of pipelines. A pipe segment (after `|`/`|&`) MUST
contribute a real head — `cat f |` with nothing (or only assignments)
after the pipe is a syntax error in real bash, not a silently-empty
command. `requireHead` is false only for the pipeline's own first segment
(shared with AndOr/List positions, where an empty/assignment-only simple
command IS legitimate, e.g. `FOO=1` alone). -/
partial def parsePipeline (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState)
    (requireHead : Bool := false) : Option (Nat × DecompState) :=
  let headsBefore := st.heads.size
  match parseCommand cs pos inSubst { st with hdTrail := false } with
  | none => none
  | some (pos1, st1) =>
    if requireHead && st1.heads.size == headsBefore then none
    else
      let pos2 := skipBlanks cs pos1
      let piped := pos2 < cs.size && cs[pos2]! == '|' &&
        !(pos2 + 1 < cs.size && cs[pos2+1]! == '|')
      -- A heredoc with trailing content in a piped command is a tree-sitter
      -- parse error in the oracle (and mvdan/sh rejects it too): fail closed.
      if piped && st1.hdTrail then none
      else if pos2 + 1 < cs.size && cs[pos2]! == '|' && cs[pos2+1]! == '&' then
        if st1.hdTrail then none else parsePipeline cs (pos2 + 2) inSubst st1 true
      else if piped then
        parsePipeline cs (pos2 + 1) inSubst st1 true
      else some (pos2, st1)

partial def parseAndOr (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  match parsePipeline cs pos inSubst st with
  | none => none
  | some (pos1, st1) =>
    let pos2 := skipBlanks cs pos1
    if pos2 + 1 < cs.size && cs[pos2]! == '&' && cs[pos2+1]! == '&' then
      parseAndOr cs (pos2 + 2) inSubst st1
    else if pos2 + 1 < cs.size && cs[pos2]! == '|' && cs[pos2+1]! == '|' then
      parseAndOr cs (pos2 + 2) inSubst st1
    else some (pos2, st1)

/-- A `List`: and-or chains separated by `;`, `&` (background), or
newline, stopping at end of input or (inside a substitution) the matching
`)`. -/
partial def parseList (cs : Array Char) (pos : Nat) (inSubst : Bool) (st : DecompState) :
    Option (Nat × DecompState) :=
  match parseAndOr cs pos inSubst st with
  | none => none
  | some (pos1, st1) =>
    let pos2 := skipBlanks cs pos1
    if pos2 >= cs.size then some (pos2, st1)
    else if inSubst && cs[pos2]! == ')' then some (pos2, st1)
    else
      let c := cs[pos2]!
      if c == ';' then parseList cs (pos2 + 1) inSubst st1
      else if c == '\n' then
        -- crossing a newline: any heredoc declared on this line now has its body
        -- immediately after. Consume all pending bodies, then continue.
        if st1.pending.isEmpty then parseList cs (pos2 + 1) inSubst st1
        else
          match consumeHeredocs cs (pos2 + 1) st1.pending 0 with
          | none => none
          | some pos3 => parseList cs pos3 inSubst { st1 with pending := #[] }
      else if c == '&' && !(pos2 + 1 < cs.size && cs[pos2+1]! == '>') then
        parseList cs (pos2 + 1) inSubst st1
      else if isTerminatorKw (peekKw cs pos2) then some (pos2, st1)  -- compound delimiter: stop, hand back
      else none  -- unexpected leftover token: malformed

end

structure Decomposition where
  ok : Bool
  heads : Array String := #[]
  reads : Array String := #[]
  writes : Array String := #[]
  -- Informative for the decompose wire kind (never compared there); load-
  -- bearing for Verify.lean's shell_allow_decomposition path.
  commands : Array String := #[]

def decomposeCommand (command : String) : Decomposition :=
  let cs := command.toList.toArray
  match parseList cs 0 false {} with
  | none => { ok := false }
  | some (pos, st) =>
    if pos != cs.size then { ok := false }
    else if !st.pending.isEmpty then { ok := false }  -- unterminated heredoc
    else if st.heads.isEmpty then { ok := false }
    else { ok := true, heads := st.heads, reads := st.reads, writes := st.writes,
           commands := st.commands }

end DaisugiVerify
