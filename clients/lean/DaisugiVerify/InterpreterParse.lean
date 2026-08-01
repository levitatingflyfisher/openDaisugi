/-
Port of interpreter_parse.py: POSIX `shlex.split`/`shlex.quote`, then the
per-interpreter payload extraction (`sh -c`, `xargs`, `find -exec`, `env`,
the ADR-0014 transparent wrappers) used by the verify-Core permission stage
to recurse into interpreter payloads. Used only by `Verify.lean` — the
decompose kind never touches this module (shell_decompose.py's tree-sitter
grammar is a completely different code path from shlex).
-/
import DaisugiVerify.Basic

namespace DaisugiVerify

private def isShlexWhitespace (c : Char) : Bool :=
  c == ' ' || c == '\t' || c == '\r' || c == '\n'

/-- POSIX `shlex.split(s, posix=True)`, default settings (whitespace_split,
quotes `'"`, escape `\`, escapedquotes `"`). `none` on an unterminated
quote (mirrors `shlex.split` raising `ValueError`, which
`parse_interpreter` catches and treats as "not an interpreter"). -/
partial def shlexSplit (s : String) : Option (List String) :=
  go s.toList [] none
where
  -- `toks`: completed tokens, most-recent-first (reversed at the end).
  -- `cur`: the in-progress token (`none` = no token started here; `some
  -- buf` even with `buf = []` once a quote has opened, so `''` still
  -- yields an empty-string token).
  go (cs : List Char) (toks : List String) (cur : Option (List Char)) : Option (List String) :=
    match cs with
    | [] =>
      match cur with
      | some buf => some (((String.ofList buf.reverse) :: toks).reverse)
      | none => some toks.reverse
    | c :: rest =>
      if isShlexWhitespace c then
        match cur with
        | some buf => go rest (String.ofList buf.reverse :: toks) none
        | none => go rest toks none
      else if c == '\'' then
        singleQuoted rest toks (cur.getD [])
      else if c == '"' then
        doubleQuoted rest toks (cur.getD [])
      else if c == '\\' then
        match rest with
        | [] => none  -- trailing backslash: shlex treats as unterminated too
        | '\n' :: rest' => go rest' toks cur  -- line continuation, no token char
        | nc :: rest' => go rest' toks (some (nc :: cur.getD []))
      else
        go rest toks (some (c :: cur.getD []))
  singleQuoted (cs : List Char) (toks : List String) (buf : List Char) : Option (List String) :=
    match cs with
    | [] => none
    | '\'' :: rest => go rest toks (some buf)
    | c :: rest => singleQuoted rest toks (c :: buf)
  doubleQuoted (cs : List Char) (toks : List String) (buf : List Char) : Option (List String) :=
    match cs with
    | [] => none
    | '"' :: rest => go rest toks (some buf)
    | '\\' :: ('"' :: rest) => doubleQuoted rest toks ('"' :: buf)
    | '\\' :: ('\\' :: rest) => doubleQuoted rest toks ('\\' :: buf)
    | '\\' :: rest => doubleQuoted rest toks ('\\' :: buf)  -- backslash kept literally
    | c :: rest => doubleQuoted rest toks (c :: buf)

/-- Python `shlex.quote`: a token built only from word chars plus
`@ % + = : , . -` and `/` (ASCII) is "safe" and returned unchanged; anything
else is wrapped in single quotes with embedded `'` replaced by `'"'"'`. The
empty string is unsafe (becomes `''`). -/
def shlexQuote (s : String) : String :=
  let safeChar (c : Char) : Bool :=
    c.isAlpha || c.isDigit || c == '_' || c == '@' || c == '%' || c == '+' ||
      c == '=' || c == ':' || c == ',' || c == '.' || c == '/' || c == '-'
  if !s.isEmpty && s.toList.all safeChar then s
  else
    let escaped := s.toList.foldl (init := ([] : List Char))
      (fun acc c => if c == '\'' then acc ++ ['\'', '"', '\'', '"', '\''] else acc ++ [c])
    "'" ++ String.ofList escaped ++ "'"

def shlexQuoteJoin (toks : List String) : String :=
  String.intercalate " " (toks.map shlexQuote)

structure InterpreterPayload where
  head : String
  innerCommands : List String := []
  isOpaque : Bool := false
  deriving Repr, BEq

def shellCInterpreters : List String :=
  ["sh", "bash", "zsh", "dash", "ksh", "fish", "csh", "tcsh"]

def opaqueInterpreters : List String :=
  ["python", "python3", "python2", "perl", "ruby", "node", "deno", "awk", "gawk",
   "sed", "make", "eval", "exec", "source", "sudo", "doas", "watch"]

def allInterpreters : List String :=
  shellCInterpreters ++ opaqueInterpreters ++
    ["xargs", "find", "env", "timeout", "nice", "nohup", "time", "stdbuf",
     "command", "setsid", "ionice"]

/-- `head [FLAGS] DURATION? CMD…` wrappers: (value-flags, positional-skip). -/
def transparentWrappers : List (String × List String × Nat) :=
  [("timeout", ["-k", "--kill-after", "-s", "--signal"], 1),
   ("nice", ["-n", "--adjustment"], 0),
   ("nohup", [], 0),
   ("time", [], 0),
   ("stdbuf", ["-i", "-o", "-e"], 0),
   ("command", [], 0),
   ("setsid", [], 0),
   ("ionice", ["-c", "-n", "-t"], 0)]

def xargsValueFlags : List String :=
  ["-n", "-I", "-P", "-L", "-d", "-E", "-s", "-a", "--max-args", "--replace",
   "--max-procs", "--max-lines", "--delimiter", "--eof", "--max-chars", "--arg-file"]

def findExecFlags : List String := ["-exec", "-execdir", "-ok", "-okdir"]

/-- `sh -c "SCRIPT"` (and clustered short flags `-ec`, `-lc`, attached
`-cSCRIPT`). Over-detection (treating any single-dash cluster containing
`c` as carrying `-c`) is the safe direction — see interpreter_parse.py. -/
def parseShellC (head : String) (tokens : List String) : InterpreterPayload :=
  let rec scan : List String → InterpreterPayload
    | [] => { head }
    | tok :: rest =>
      let cs := tok.toList
      match cs with
      | '-' :: c2 :: crest =>
        if c2 == '-' then scan rest  -- long option, not a short cluster
        else
          let cluster := c2 :: crest
          match cluster.findIdx? (· == 'c') with
          | none => scan rest
          | some idx =>
            let before := cluster.take idx
            if !before.isEmpty && !before.all Char.isAlpha then scan rest
            else
              let attached := cluster.drop (idx + 1)
              if !attached.isEmpty then { head, innerCommands := [String.ofList attached] }
              else
                match rest with
                | nxt :: _ => { head, innerCommands := [nxt] }
                | [] => { head }
      | _ => scan rest
  scan tokens

def skipFlagsAndPositionals (valueFlags : List String) (positionalSkip : Nat) (tokens : List String) :
    List String :=
  let rec skipFlags : List String → List String
    | [] => []
    | "--" :: rest => rest
    | t :: rest =>
      if t.length > 0 && t.front == '-' && t != "-" then
        if valueFlags.contains t then
          match rest with
          | _ :: rest' => skipFlags rest'
          | [] => []
        else skipFlags rest
      else t :: rest
  let afterFlags := skipFlags tokens
  afterFlags.drop positionalSkip

def parseWrapper (head : String) (valueFlags : List String) (positionalSkip : Nat)
    (tokens : List String) : InterpreterPayload :=
  match skipFlagsAndPositionals valueFlags positionalSkip tokens with
  | [] => { head }
  | rem => { head, innerCommands := [shlexQuoteJoin rem] }

def parseXargs (tokens : List String) : InterpreterPayload :=
  let rec skip : List String → List String
    | [] => []
    | "--" :: rest => rest
    | t :: rest =>
      if t.length > 0 && t.front == '-' then
        if xargsValueFlags.contains t then
          match rest with
          | _ :: rest' => skip rest'
          | [] => []
        else skip rest
      else t :: rest
  match skip tokens with
  | [] => { head := "xargs" }
  | rem => { head := "xargs", innerCommands := [shlexQuoteJoin rem] }

partial def findExecInners (ts : List String) (inners : List String) : List String :=
  match ts with
  | [] => inners
  | t :: rest =>
    if findExecFlags.contains t then
      let (body, tail) := rest.span (fun x => x != ";" && x != "+")
      let inners' := if body.isEmpty then inners else inners ++ [shlexQuoteJoin body]
      findExecInners (tail.drop 1) inners'
    else findExecInners rest inners

def parseFind (tokens : List String) : InterpreterPayload :=
  { head := "find", innerCommands := findExecInners tokens [] }

def parseEnv (tokens : List String) : InterpreterPayload :=
  let rec skip : List String → List String
    | [] => []
    | t :: rest =>
      if t.length > 0 && t.front == '-' then skip rest
      else if t.toList.contains '=' && t.front != '=' then skip rest
      else t :: rest
  match skip tokens with
  | [] => { head := "env" }
  | rem => { head := "env", innerCommands := [shlexQuoteJoin rem] }

/-- Port of `parse_interpreter`: `none` when the head isn't a recognized
interpreter, or when `shlexSplit` fails (unbalanced quotes). -/
def parseInterpreter (command : String) : Option InterpreterPayload :=
  let stripped := pyStrip command
  if stripped.isEmpty then none
  else match shlexSplit stripped with
    | none => none
    | some [] => none
    | some (head :: rest) =>
      if !allInterpreters.contains head then none
      else if opaqueInterpreters.contains head then some { head, isOpaque := true }
      else if shellCInterpreters.contains head then some (parseShellC head rest)
      else if head == "xargs" then some (parseXargs rest)
      else if head == "find" then some (parseFind rest)
      else if head == "env" then some (parseEnv rest)
      else
        match transparentWrappers.find? (fun (h, _, _) => h == head) with
        | some (_, vf, ps) => some (parseWrapper head vf ps rest)
        | none => some { head, isOpaque := true }

end DaisugiVerify
