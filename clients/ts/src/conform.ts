#!/usr/bin/env node
/**
 * The wire-protocol entrypoint: one case JSON per line on stdin, one verdict
 * JSON per line on stdout, flushed per line, order-independent (matched by
 * id). Never aborts the stream on a per-case failure — emits {id, error}.
 * See docs/spec/conformance.md.
 */

import readline from "node:readline";
import { getParser, decomposeCommand } from "./shellDecompose.js";
import { verifyCase } from "./verify.js";
import { closeZ3 } from "./z3client.js";

const CONFORMANCE_VERSION = 1;

interface Verdict {
  id: string | null;
  ok?: boolean;
  violations?: { stage: string; step: string | null }[];
  heads?: string[];
  commands?: string[];
  reads?: string[];
  writes?: string[];
  error?: string;
}

async function handleCase(raw: string): Promise<Verdict> {
  let parsed: any;
  try {
    parsed = JSON.parse(raw);
  } catch (e: any) {
    return { id: null, error: `invalid JSON: ${e?.message ?? e}` };
  }
  const id: string | null = parsed?.id ?? null;
  try {
    if (typeof parsed.v === "number" && parsed.v > CONFORMANCE_VERSION) {
      return { id, error: `unsupported conformance version ${parsed.v}` };
    }
    if (parsed.kind === "decompose") {
      const d = await decomposeCommand(parsed.command);
      if (!d.ok) return { id, ok: false };
      return {
        id,
        ok: true,
        heads: d.heads,
        commands: d.commands,
        reads: [...d.reads].sort(),
        writes: [...d.writes].sort(),
      };
    }
    if (parsed.kind === "verify") {
      const result = await verifyCase(parsed);
      return {
        id,
        ok: result.ok,
        violations: result.violations.map((v) => ({
          stage: v.stage,
          step: (v.detail?.step ?? null) as string | null,
        })),
      };
    }
    return { id, error: `unknown kind ${JSON.stringify(parsed.kind)}` };
  } catch (e: any) {
    return { id, error: (e?.stack ?? String(e)).slice(0, 300) };
  }
}

async function main(): Promise<void> {
  // Async wasm init BEFORE consuming stdin — no buffering surprises.
  await getParser();

  const rl = readline.createInterface({ input: process.stdin, terminal: false });
  for await (const line of rl) {
    if (!line.trim()) continue;
    const verdict = await handleCase(line);
    process.stdout.write(JSON.stringify(verdict) + "\n");
  }
  // The z3 subprocess (if ever spawned) is long-lived by design — release it
  // explicitly so the event loop can drain and the process exits, matching
  // the wire protocol's "exit non-zero only for process-level failure" (here:
  // exit cleanly on EOF).
  closeZ3();
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
