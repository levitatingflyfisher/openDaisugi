/**
 * Port of CPython 3.12 `shlex.split(s, comments=False, posix=True)` and
 * `shlex.quote`. Only the reachable configuration is ported (posix=True,
 * whitespace_split=True, commenters='', no punctuation_chars) — the general
 * shlex class has many modes the oracle never exercises.
 *
 * `shlexSplitPosix` throws `ShlexError` on unbalanced quotes / a trailing
 * unescaped backslash, matching CPython's `ValueError` — callers that treat
 * a parse failure as "not an interpreter" should catch it (see
 * `interpreterParse.ts`, mirroring `parse_interpreter`'s `except ValueError`).
 */

export class ShlexError extends Error {}

const WHITESPACE = new Set([" ", "\t", "\r", "\n"]);
const QUOTES = new Set(["'", '"']);
const ESCAPE = "\\";
const ESCAPED_QUOTES = new Set(['"']); // only double-quotes honor backslash-escaping

type State = " " | "a" | "'" | '"' | "\\" | null;

export function shlexSplitPosix(s: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  const n = s.length;

  while (true) {
    let state: State = " ";
    let token = "";
    let quoted = false;
    let escapedState: State = " ";
    let sawEnd = false;

    while (true) {
      const nextchar = i < n ? s[i]! : "";
      if (nextchar !== "") i += 1;

      if (state === null) {
        token = "";
        break;
      } else if (state === " ") {
        if (nextchar === "") {
          state = null;
          break;
        } else if (WHITESPACE.has(nextchar)) {
          if (token || quoted) {
            break;
          } else {
            continue;
          }
        } else if (nextchar === ESCAPE) {
          escapedState = "a";
          state = "\\";
        } else if (QUOTES.has(nextchar)) {
          state = nextchar as State;
        } else {
          // whitespace_split catch-all (wordchars are a strict subset of
          // this in the posix+whitespace_split configuration, so folding
          // them together is behavior-preserving).
          token = nextchar;
          state = "a";
        }
      } else if (state === "'" || state === '"') {
        quoted = true;
        if (nextchar === "") {
          throw new ShlexError("No closing quotation");
        }
        if (nextchar === state) {
          state = "a";
        } else if (nextchar === ESCAPE && ESCAPED_QUOTES.has(state)) {
          escapedState = state;
          state = "\\";
        } else {
          token += nextchar;
        }
      } else if (state === "\\") {
        if (nextchar === "") {
          throw new ShlexError("No escaped character");
        }
        if (
          escapedState !== null &&
          QUOTES.has(escapedState) &&
          nextchar !== state &&
          nextchar !== escapedState
        ) {
          token += state; // preserve the backslash literally
        }
        token += nextchar;
        state = escapedState;
      } else if (state === "a") {
        if (nextchar === "") {
          state = null;
          break;
        } else if (WHITESPACE.has(nextchar)) {
          state = " ";
          if (token || quoted) {
            break;
          } else {
            continue;
          }
        } else if (QUOTES.has(nextchar)) {
          state = nextchar as State;
        } else if (nextchar === ESCAPE) {
          escapedState = "a";
          state = "\\";
        } else {
          token += nextchar;
        }
      }
    }

    let result: string | null = token;
    if (!quoted && result === "") {
      result = null;
    }
    if (result === null) {
      sawEnd = true;
    } else {
      tokens.push(result);
    }
    if (sawEnd) break;
  }

  return tokens;
}

// shlex.quote — mirrors CPython's `_find_unsafe` (ASCII: not in [\w@%+=:,./-]).
const UNSAFE_RE = /[^A-Za-z0-9_@%+=:,./-]/;

export function shlexQuote(s: string): string {
  if (s === "") return "''";
  if (!UNSAFE_RE.test(s)) return s;
  return "'" + s.split("'").join("'\"'\"'") + "'";
}
