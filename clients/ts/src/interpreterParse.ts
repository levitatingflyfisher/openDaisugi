/**
 * Port of interpreter_parse.py — shell-interpreter payload extraction
 * (sh -c / xargs / find -exec / env / the ADR-0014 transparent wrappers).
 */

import { SHELL_INTERPRETERS } from "./models.js";
import { shlexSplitPosix, shlexQuote, ShlexError } from "./shlex.js";

const SHELL_C_INTERPRETERS = new Set(["sh", "bash", "zsh", "dash", "ksh", "fish", "csh", "tcsh"]);

const OPAQUE_INTERPRETERS = new Set([
  "python",
  "python3",
  "python2",
  "perl",
  "ruby",
  "node",
  "deno",
  "awk",
  "gawk",
  "sed",
  "make",
  "eval",
  "exec",
  "source",
  "sudo",
  "doas",
  "watch",
]);

// head -> (value flags that consume the next token, positional args to skip before CMD)
const TRANSPARENT_WRAPPERS: Record<string, { valueFlags: Set<string>; positionalSkip: number }> = {
  timeout: { valueFlags: new Set(["-k", "--kill-after", "-s", "--signal"]), positionalSkip: 1 },
  nice: { valueFlags: new Set(["-n", "--adjustment"]), positionalSkip: 0 },
  nohup: { valueFlags: new Set(), positionalSkip: 0 },
  time: { valueFlags: new Set(), positionalSkip: 0 },
  stdbuf: { valueFlags: new Set(["-i", "-o", "-e"]), positionalSkip: 0 },
  command: { valueFlags: new Set(), positionalSkip: 0 },
  setsid: { valueFlags: new Set(), positionalSkip: 0 },
  ionice: { valueFlags: new Set(["-c", "-n", "-t"]), positionalSkip: 0 },
};

const XARGS_VALUE_FLAGS = new Set([
  "-n",
  "-I",
  "-P",
  "-L",
  "-d",
  "-E",
  "-s",
  "-a",
  "--max-args",
  "--replace",
  "--max-procs",
  "--max-lines",
  "--delimiter",
  "--eof",
  "--max-chars",
  "--arg-file",
]);

const FIND_EXEC_FLAGS = new Set(["-exec", "-execdir", "-ok", "-okdir"]);

export interface InterpreterPayload {
  head: string;
  innerCommands: string[];
  opaque: boolean;
}

function payload(head: string, innerCommands: string[] = [], opaque = false): InterpreterPayload {
  return { head, innerCommands, opaque };
}

export function parseInterpreter(command: string): InterpreterPayload | null {
  const stripped = command.trim();
  if (!stripped) return null;
  let tokens: string[];
  try {
    tokens = shlexSplitPosix(stripped);
  } catch (e) {
    if (e instanceof ShlexError) return null;
    throw e;
  }
  if (tokens.length === 0) return null;
  const head = tokens[0]!;
  if (!SHELL_INTERPRETERS.has(head)) return null;
  if (OPAQUE_INTERPRETERS.has(head)) return payload(head, [], true);
  if (SHELL_C_INTERPRETERS.has(head)) return parseShellC(head, tokens);
  if (head === "xargs") return parseXargs(head, tokens);
  if (head === "find") return parseFind(head, tokens);
  if (head === "env") return parseEnv(head, tokens);
  if (head in TRANSPARENT_WRAPPERS) return parseWrapper(head, tokens);
  return payload(head, [], true);
}

function parseShellC(head: string, tokens: string[]): InterpreterPayload {
  for (let i = 1; i < tokens.length; i++) {
    const tok = tokens[i]!;
    if (tok.length < 2 || tok[0] !== "-" || tok[1] === "-" || !tok.includes("c")) continue;
    const cluster = tok.slice(1);
    const cpos = cluster.indexOf("c");
    const before = cluster.slice(0, cpos);
    if (before && !/^[A-Za-z]+$/.test(before)) continue;
    const attached = cluster.slice(cpos + 1);
    if (attached) {
      return payload(head, [attached]);
    }
    if (i + 1 < tokens.length) {
      return payload(head, [tokens[i + 1]!]);
    }
    return payload(head, []);
  }
  return payload(head, []);
}

function parseWrapper(head: string, tokens: string[]): InterpreterPayload {
  const { valueFlags, positionalSkip } = TRANSPARENT_WRAPPERS[head]!;
  let i = 1;
  while (i < tokens.length) {
    const t = tokens[i]!;
    if (t === "--") {
      i += 1;
      break;
    }
    if (t.startsWith("-") && t !== "-") {
      if (valueFlags.has(t) && i + 1 < tokens.length) {
        i += 2;
        continue;
      }
      i += 1;
      continue;
    }
    break;
  }
  i += Math.min(positionalSkip, Math.max(0, tokens.length - i));
  if (i < tokens.length) {
    const inner = tokens
      .slice(i)
      .map((t) => shlexQuote(t))
      .join(" ");
    return payload(head, [inner]);
  }
  return payload(head, []);
}

function parseXargs(head: string, tokens: string[]): InterpreterPayload {
  let i = 1;
  while (i < tokens.length) {
    const t = tokens[i]!;
    if (t === "--") {
      i += 1;
      break;
    }
    if (t.startsWith("-")) {
      if (XARGS_VALUE_FLAGS.has(t) && i + 1 < tokens.length) {
        i += 2;
        continue;
      }
      i += 1;
      continue;
    }
    break;
  }
  if (i < tokens.length) {
    const inner = tokens
      .slice(i)
      .map((t) => shlexQuote(t))
      .join(" ");
    return payload(head, [inner]);
  }
  return payload(head, []);
}

function parseFind(head: string, tokens: string[]): InterpreterPayload {
  const inners: string[] = [];
  let i = 0;
  while (i < tokens.length) {
    if (FIND_EXEC_FLAGS.has(tokens[i]!)) {
      const start = i + 1;
      let j = start;
      while (j < tokens.length && tokens[j] !== ";" && tokens[j] !== "+") j += 1;
      if (j > start) {
        const inner = tokens
          .slice(start, j)
          .map((t) => shlexQuote(t))
          .join(" ");
        inners.push(inner);
      }
      i = j + 1;
    } else {
      i += 1;
    }
  }
  return payload(head, inners);
}

function parseEnv(head: string, tokens: string[]): InterpreterPayload {
  let i = 1;
  while (i < tokens.length) {
    const t = tokens[i]!;
    if (t.startsWith("-")) {
      i += 1;
      continue;
    }
    if (t.includes("=") && !t.startsWith("=")) {
      i += 1;
      continue;
    }
    break;
  }
  if (i < tokens.length) {
    const inner = tokens
      .slice(i)
      .map((t) => shlexQuote(t))
      .join(" ");
    return payload(head, [inner]);
  }
  return payload(head, []);
}
