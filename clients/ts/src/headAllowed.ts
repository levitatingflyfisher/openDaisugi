/**
 * Port of verify.py's `_head_allowed` — the LEFT-anchored, segment-count-equal
 * glob semantics used for the shell allowlist and `mcp_allowlist`. Deliberately
 * NOT the same algorithm as `pathMatchesAny` (see PORTING-NOTES.md "the two
 * glob semantics").
 */

import { fnmatchcase } from "./pyport.js";

const GLOB_CHARS_RE = /[*?[]/;

export function headAllowed(head: string, allowlist: string[]): boolean {
  for (const pat of allowlist) {
    if (head === pat) return true;
    if (!GLOB_CHARS_RE.test(pat)) continue;
    const headSegs = head.split("/");
    const patSegs = pat.split("/");
    if (headSegs.length !== patSegs.length) continue;
    if (headSegs.every((h, idx) => fnmatchcase(h, patSegs[idx]!))) return true;
  }
  return false;
}
