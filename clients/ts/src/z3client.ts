/**
 * A single long-lived `z3 -in` subprocess speaking SMT-LIB2 text, per the
 * Full-profile house rule (never link a solver API). One `(check-sat)` per
 * request, wrapped in `(push)`/`(pop)` so declarations and assertions from
 * one query never leak into the next. Never calls `(get-model)` — every
 * consumer here only needs the sat/unsat/unknown verdict; counterexample
 * text is informative and out of scope (see PORTING-NOTES / advisor notes).
 *
 * Spawned lazily and only for cases that actually need Z3 (an
 * expr-bearing invariant/postcondition, or a SkillStep) — the 13,084
 * decompose cases and the ~277 plain-permissions verify cases never touch
 * this process, so the bench stays honest.
 */

import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import readline from "node:readline";

export type SatResult = "sat" | "unsat" | "unknown";

export class VerificationTimeout extends Error {}

class Z3Session {
  private proc: ChildProcessWithoutNullStreams;
  private rl: readline.Interface;
  private pending: ((line: string) => void)[] = [];
  private closed = false;

  constructor(binary = "z3") {
    this.proc = spawn(binary, ["-in"], { stdio: ["pipe", "pipe", "pipe"] });
    this.proc.on("error", (e) => {
      // Surface spawn failures (binary not on PATH) to whoever is waiting.
      const resolve = this.pending.shift();
      if (resolve) resolve(`__spawn_error__:${e.message}`);
    });
    this.rl = readline.createInterface({ input: this.proc.stdout });
    this.rl.on("line", (line) => {
      const resolve = this.pending.shift();
      if (resolve) resolve(line);
    });
  }

  private nextLine(): Promise<string> {
    return new Promise((resolve) => this.pending.push(resolve));
  }

  /** Run `script` (declarations + assertions, no push/pop/check-sat of its
   * own) in an isolated push/pop frame and return the check-sat verdict. */
  async checkSat(script: string, timeoutMs: number): Promise<SatResult> {
    if (this.closed) throw new Error("z3 session closed");
    const full = `(push)\n(set-option :timeout ${Math.max(1, Math.floor(timeoutMs))})\n${script}\n(check-sat)\n(pop)\n`;
    const linePromise = this.nextLine();
    this.proc.stdin.write(full);
    const line = (await linePromise).trim();
    if (line.startsWith("__spawn_error__:")) {
      throw new Error(`z3 spawn failed: ${line.slice("__spawn_error__:".length)}`);
    }
    if (line === "sat") return "sat";
    if (line === "unsat") return "unsat";
    return "unknown"; // "unknown", "timeout", or anything unexpected — fail toward "can't prove"
  }

  close(): void {
    this.closed = true;
    this.rl.close();
    this.proc.stdin.end();
    this.proc.kill();
  }
}

let session: Z3Session | null = null;

function getSession(): Z3Session {
  if (session === null) session = new Z3Session();
  return session;
}

export async function z3CheckSat(script: string, timeoutMs: number): Promise<SatResult> {
  return getSession().checkSat(script, timeoutMs);
}

export function closeZ3(): void {
  if (session !== null) {
    session.close();
    session = null;
  }
}

/** SMT-LIB2 string literal escaping (Z3's string theory: `"` doubles to `""`). */
export function smtStringLit(s: string): string {
  return `"${s.replace(/"/g, '""')}"`;
}
