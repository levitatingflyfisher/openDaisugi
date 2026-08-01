import type { Stakes } from "./models.js";

const STRICT_STAKES: ReadonlySet<Stakes> = new Set(["high", "physical"]);

export function resolveStrict(strict: boolean | null, stakes: Stakes): boolean {
  if (strict !== null) return strict;
  return STRICT_STAKES.has(stakes);
}
