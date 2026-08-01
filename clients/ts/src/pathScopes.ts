/**
 * Port of verify.py's `_path_matches_any` / `_match_glob` (file_read/
 * file_write scope checking) — the oracle's NATIVE left-anchored, /-aware
 * matcher (post F-1/F-2 fix; see clients/ADJUDICATIONS.md).
 *
 * F-1/F-2 were frozen-then-fixed oracle-side: `PurePosixPath.match` matched
 * relative patterns from the RIGHT (`file_write: ["out.txt"]` admitted
 * `/etc/cron.d/out.txt` — a scope escape) and mid-pattern `**` changed
 * meaning between Python 3.12/3.13. The oracle now implements matching
 * natively: a pattern must consume the WHOLE path (left- AND right-anchored),
 * so a relative pattern never matches an absolute path, and `**` recursively
 * spans zero or more segments identically on every Python version.
 * `*`/`?`/`[...]` stay within one segment (via fnmatchcase).
 */

import { normpath, fnmatchcase } from "./pyport.js";

function matchGlob(norm: string, glob: string): boolean {
  if (glob.endsWith("/**")) {
    const raw = glob.slice(0, -3);
    if (raw === "") {
      // "/**" — the root: any absolute path.
      return norm.startsWith("/");
    }
    const prefix = normpath(raw);
    if (prefix === ".") {
      // "./**" — any relative path (never an absolute one).
      return !norm.startsWith("/") && norm !== ".." && !norm.startsWith("../");
    }
    return norm === prefix || norm.startsWith(prefix + "/");
  }

  const patSegs = glob.split("/");
  const pathSegs = norm.split("/");

  function matchFrom(pi: number, ti: number): boolean {
    if (pi === patSegs.length) {
      return ti === pathSegs.length;
    }
    if (patSegs[pi] === "**") {
      for (let k = ti; k <= pathSegs.length; k++) {
        if (matchFrom(pi + 1, k)) return true;
      }
      return false;
    }
    if (ti === pathSegs.length) {
      return false;
    }
    if (fnmatchcase(pathSegs[ti]!, patSegs[pi]!)) {
      return matchFrom(pi + 1, ti + 1);
    }
    return false;
  }

  return matchFrom(0, 0);
}

export function pathMatchesAny(path: string, globs: string[]): boolean {
  const normalized = normpath(path);
  return globs.some((g) => matchGlob(normalized, g));
}
