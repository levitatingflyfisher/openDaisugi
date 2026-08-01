/**
 * Hand-written ports of the CPython 3.12 stdlib semantics the oracle leans
 * on: `posixpath.normpath`/`splitroot` and `fnmatch.translate`/`fnmatchcase`.
 *
 * Validated against `_head_allowed` test vectors generated directly from the
 * oracle (not hand-picked), not derived from documentation. See
 * clients/ts/README.md for the generation recipe.
 *
 * `normpath` is also the basis for `pathScopes.ts`'s file-scope matcher
 * (`_path_matches_any`/`_match_glob`) — that matcher used to delegate to a
 * `PurePosixPath.match` port living in this file (Python 3.12's "lines"
 * trick), until the oracle replaced its own `PurePosixPath`-based matcher
 * with a native left-anchored one (see clients/ADJUDICATIONS.md F-1/F-2).
 * The pathlib-emulation code was removed here to match — `pathScopes.ts`
 * now does its own left-anchored, `/`-aware, backtracking segment match
 * using only `normpath` + `fnmatchcase` from this file.
 */

// --- posixpath.splitroot / normpath -----------------------------------------

export function splitroot(p: string): { root: string; rel: string } {
  if (p.slice(0, 1) !== "/") {
    return { root: "", rel: p };
  }
  if (p.slice(1, 2) !== "/" || p.slice(2, 3) === "/") {
    // Absolute path, e.g. '/foo', '///foo', '////foo'.
    return { root: "/", rel: p.slice(1) };
  }
  // Precisely two leading slashes, e.g. '//foo'.
  return { root: p.slice(0, 2), rel: p.slice(2) };
}

export function normpath(path: string): string {
  if (path === "") return ".";
  const { root: initialSlashes, rel } = splitroot(path);
  const comps = rel.split("/");
  const newComps: string[] = [];
  for (const comp of comps) {
    if (comp === "" || comp === ".") continue;
    if (
      comp !== ".." ||
      (initialSlashes === "" && newComps.length === 0) ||
      (newComps.length > 0 && newComps[newComps.length - 1] === "..")
    ) {
      newComps.push(comp);
    } else if (newComps.length > 0) {
      newComps.pop();
    }
  }
  const joined = initialSlashes + newComps.join("/");
  return joined || ".";
}

// --- re.escape (single character, matching CPython's _special_chars_map) ---

const RE_SPECIAL = new Set(
  "()[]{}?*+-|^$\\.&~# \t\n\r\v\f".split("")
);

function reEscapeChar(c: string): string {
  return RE_SPECIAL.has(c) ? "\\" + c : c;
}

// --- fnmatch.translate (core, without the "(?s:" / ")\Z" wrap) -------------

const STAR = Symbol("STAR");

function fnmatchTranslateParts(pat: string): (string | typeof STAR)[] {
  const res: (string | typeof STAR)[] = [];
  let i = 0;
  const n = pat.length;
  while (i < n) {
    const c = pat[i]!;
    i += 1;
    if (c === "*") {
      if (res.length === 0 || res[res.length - 1] !== STAR) res.push(STAR);
    } else if (c === "?") {
      res.push(".");
    } else if (c === "[") {
      let j = i;
      if (j < n && pat[j] === "!") j += 1;
      if (j < n && pat[j] === "]") j += 1;
      while (j < n && pat[j] !== "]") j += 1;
      if (j >= n) {
        res.push("\\[");
      } else {
        let stuff = pat.slice(i, j);
        if (!stuff.includes("-")) {
          stuff = stuff.split("\\").join("\\\\");
        } else {
          const chunks: string[] = [];
          let k = pat[i] === "!" ? i + 2 : i + 1;
          let ii = i;
          while (true) {
            k = pat.indexOf("-", k);
            if (k < 0 || k >= j) {
              k = -1;
              break;
            }
            chunks.push(pat.slice(ii, k));
            ii = k + 1;
            k = k + 3;
          }
          const chunk = pat.slice(ii, j);
          if (chunk) {
            chunks.push(chunk);
          } else if (chunks.length > 0) {
            chunks[chunks.length - 1] = chunks[chunks.length - 1] + "-";
          } else {
            chunks.push("-");
          }
          for (let kk = chunks.length - 1; kk > 0; kk--) {
            const prev = chunks[kk - 1]!;
            const cur = chunks[kk]!;
            if (prev.length > 0 && cur.length > 0 && prev[prev.length - 1]! > cur[0]!) {
              chunks[kk - 1] = prev.slice(0, -1) + cur.slice(1);
              chunks.splice(kk, 1);
            }
          }
          stuff = chunks
            .map((s) => s.split("\\").join("\\\\").split("-").join("\\-"))
            .join("-");
        }
        stuff = stuff.replace(/([&~|])/g, "\\$1");
        i = j + 1;
        if (!stuff) {
          res.push("(?!)");
        } else if (stuff === "!") {
          res.push(".");
        } else {
          if (stuff[0] === "!") {
            stuff = "^" + stuff.slice(1);
          } else if (stuff[0] === "^" || stuff[0] === "[") {
            stuff = "\\" + stuff;
          }
          res.push(`[${stuff}]`);
        }
      }
    } else {
      res.push(reEscapeChar(c));
    }
  }
  return res;
}

/** fnmatch.translate(pat), sliced to just the core (no "(?s:" wrap / ")\Z"). */
export function fnmatchTranslateCore(pat: string): string {
  const inp = fnmatchTranslateParts(pat);
  const out: string[] = [];
  let i = 0;
  const n = inp.length;
  while (i < n && inp[i] !== STAR) {
    out.push(inp[i] as string);
    i += 1;
  }
  while (i < n) {
    // inp[i] is STAR
    i += 1;
    if (i === n) {
      out.push(".*");
      break;
    }
    const fixed: string[] = [];
    while (i < n && inp[i] !== STAR) {
      fixed.push(inp[i] as string);
      i += 1;
    }
    const fixedStr = fixed.join("");
    if (i === n) {
      out.push(".*");
      out.push(fixedStr);
    } else {
      // Atomic groups don't change match/no-match outcomes for our
      // anchored, full-consumption use (no captures involved) — using a
      // plain non-atomic group is semantically equivalent here and avoids
      // depending on newer regex engine features.
      out.push(`(?:.*?${fixedStr})`);
    }
  }
  return out.join("");
}

/** fnmatch.fnmatchcase(name, pat) — full, case-sensitive, DOTALL match. */
export function fnmatchcase(name: string, pat: string): boolean {
  const core = fnmatchTranslateCore(pat);
  const re = new RegExp(`^(?:${core})$`, "s");
  return re.test(name);
}
