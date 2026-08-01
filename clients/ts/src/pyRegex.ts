/**
 * A narrow `re.search(pattern, s)` shim for predicate `matches`/`not_matches`
 * expressions. Python and JS regex syntax coincide for the patterns this
 * algebra actually carries (anchors, alternation, character classes,
 * `\s`/`\d`/`\w`, literals) — the one real syntax gap is Python's inline
 * flag group (`(?i)`, `(?im)`, …), which JS doesn't accept as a bare prefix.
 * We strip a LEADING inline-flag group and translate it to JS flags (i, m,
 * s); `x`/`a`/`u`/`L` are accepted but have no JS equivalent to apply
 * (verbose-mode whitespace stripping, ASCII-only classes) and are dropped —
 * none of the corpus's three regex patterns use them.
 */

const LEADING_INLINE_FLAGS_RE = /^\(\?([aiLmsux]+)\)/;

export function pyRegexSearch(pattern: string, s: string): RegExpMatchArray | null {
  let body = pattern;
  let flags = "";
  const m = LEADING_INLINE_FLAGS_RE.exec(pattern);
  if (m) {
    body = pattern.slice(m[0].length);
    for (const ch of m[1]!) {
      if (ch === "i") flags += "i";
      else if (ch === "m") flags += "m";
      else if (ch === "s") flags += "s";
      // a/u/x/L: no JS equivalent applied; not exercised by the corpus.
    }
  }
  const re = new RegExp(body, flags);
  return re.exec(s);
}
