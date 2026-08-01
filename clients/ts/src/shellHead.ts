/**
 * Port of verify.py's `_extract_shell_head`. Uses Python's bare `str.split()`
 * semantics (split on runs of whitespace, no leading empty token) via
 * `.trim().split(/\s+/)`, per PORTING-NOTES.
 */

const ENV_ASSIGN_RE = /^[A-Za-z_][A-Za-z0-9_]*=/;

export function extractShellHead(stripped: string): string | null {
  if (!stripped) return null;
  if (stripped.startsWith("#")) return null;
  const tokens = stripped.trim().split(/\s+/);
  for (const tok of tokens) {
    if (ENV_ASSIGN_RE.test(tok)) continue;
    return tok;
  }
  return null;
}
