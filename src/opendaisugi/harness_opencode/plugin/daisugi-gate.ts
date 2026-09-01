// The daisugi gate plugin for OpenCode. Every tool call asks the resident
// gate over its Unix socket before it runs. OpenCode's tool.execute.before
// hook can only deny, by a throw, so this plugin can never grant anything.
// Facts about OpenCode's plugin loader and hook shapes are recorded in
// harness/coppice/internal/adapters/opencode/PINS.md.
//
// This file exports one value only, the default plugin function. OpenCode
// calls every function a plugin file exports as a plugin, and a file whose
// export is not a function fails to load, which leaves OpenCode with no
// gate. Keep every helper below private to this module.
//
// No gate mode lives in this file. The gate resolves shadow or enforce
// itself from config.yaml, a file an agent in OpenCode cannot rewrite
// without the gate's own leave.
import type { Plugin } from "@opencode-ai/plugin";
import net from "node:net";
import fs from "node:fs";
import path from "node:path";

interface Verdict {
  allow: boolean;
  reason: string;
}

const GATE_TIMEOUT_MS = 5000;
const REPORT_TIMEOUT_MS = 1000;
const ASK_DEADLINE_S = 90;
const DETAIL_MAX = 200;
const UNREACHABLE_REASON = "openDaisugi gate unreachable: run daisugi start";
// A gate reply is one short JSON line. A longer reply is not a verdict.
const MAX_REPLY_CHARS = 1 << 20;

// OpenCode's built-in tool ids, mapped to the tool names the gate already
// classifies. Every other id, such as apply_patch, task, skill, todowrite
// or an MCP tool, goes to the gate as mcp__opencode__<id>. The gate treats
// that as an MCP call: denied unless the envelope's mcp_allowlist names
// opencode/<id>, and never an undoable ask.
const TOOL_NAMES: Record<string, string> = {
  bash: "Bash",
  read: "Read",
  write: "Write",
  edit: "Edit",
  glob: "Glob",
  grep: "Grep",
  webfetch: "WebFetch",
  websearch: "WebSearch",
};

function gateToolName(id: unknown): string {
  if (typeof id !== "string" || id === "") return "";
  return Object.hasOwn(TOOL_NAMES, id) ? TOOL_NAMES[id] : `mcp__opencode__${id}`;
}

// The directory a call runs in. OpenCode's bash tool takes a workdir
// argument, and the gate must check the command from that directory.
function callCwd(directory: string, tool: unknown, args: any): string {
  const base = typeof directory === "string" ? directory : "";
  if (tool !== "bash" || args === null || typeof args !== "object" || !("workdir" in args)) {
    return base;
  }
  if (typeof args.workdir !== "string") throw new Error("workdir is not a string");
  if (base) return path.resolve(base, args.workdir);
  // With no session directory a relative workdir has no place to start.
  if (!path.isAbsolute(args.workdir)) throw new Error("relative workdir with no directory");
  return args.workdir;
}

function gateSocketPath(env: NodeJS.ProcessEnv): string {
  if (env.OPENDAISUGI_GATE_SOCK) return env.OPENDAISUGI_GATE_SOCK;
  if (!env.HOME) return "";
  return path.join(env.HOME, ".opendaisugi", "gate", "gate.sock");
}

// This process's own coppice/Herdr pane identity, read from ITS OWN
// environment: OpenCode runs as the actual pane process (or a descendant
// of it), so its environment IS the caller's identity. Sent as explicit
// request fields on every call to the resident gate, mirroring
// gate_client.py's caller_pane_fields: the resident gate is a separate
// long-lived process with no pane environment of its own, so these are the
// only way a report it makes on this call's behalf reaches THIS pane
// rather than nobody's. Kept private to this module: OpenCode calls every
// exported function as a plugin, so nothing but the default export may be
// exported here.
function callerPaneFields(env: NodeJS.ProcessEnv): Record<string, string> {
  const fields: Record<string, string> = {};
  if (env.COPPICE_SOCK && env.COPPICE_PANE) {
    fields.coppice_sock = env.COPPICE_SOCK;
    fields.coppice_pane = env.COPPICE_PANE;
  }
  const herdrPane = env.HERDR_PANE_ID || env.HERDR_PANE;
  if (herdrPane) fields.herdr_pane = herdrPane;
  // The coppice data directory this pane's server keeps its secrets in.
  // The resident gate guards it as it guards the default one. Only an
  // absolute path is sent, since coppice always sets one.
  const dataDir = env.COPPICE_DATA_DIR;
  if (dataDir && path.isAbsolute(dataDir)) fields.coppice_data_dir = dataDir;
  return fields;
}

// Mirrors gate_client.py's _socket_is_trustworthy: the path entry must be a
// real socket, owned by this uid, mode 0600. lstat, never stat: stat follows
// symlinks, so a rogue process could plant a symlink to a socket it controls
// and have this trust it. Without this check any local process that wins the
// path answers {"exit_code":0} and every tool call is allowed.
function gateSocketTrustworthy(p: string): boolean {
  try {
    const st = fs.lstatSync(p);
    return st.isSocket() && st.uid === process.getuid!() && (st.mode & 0o777) === 0o600;
  } catch {
    return false;
  }
}

interface GateReply {
  exitCode: number;
  stderr: string;
}

function sendGateRequest(
  sockPath: string,
  argv: string[],
  payload: unknown,
  timeoutMs: number,
  extraFields: Record<string, unknown> = {}
): Promise<GateReply | null> {
  return new Promise((resolve) => {
    if (!gateSocketTrustworthy(sockPath)) {
      resolve(null);
      return;
    }
    let settled = false;
    const finish = (v: GateReply | null) => {
      if (settled) return;
      settled = true;
      resolve(v);
    };
    // Build the request before connecting, so a serialization failure is a
    // block from this promise and never an uncaught throw inside a socket
    // event handler in OpenCode's own process.
    let line: string;
    let sock: net.Socket;
    try {
      line = JSON.stringify({
        v: 1,
        argv,
        stdin_b64: Buffer.from(JSON.stringify(payload)).toString("base64"),
        ...extraFields,
      }) + "\n";
      sock = net.createConnection(sockPath);
    } catch {
      finish(null);
      return;
    }
    const timer = setTimeout(() => {
      sock.destroy();
      finish(null);
    }, timeoutMs);
    let buf = "";
    sock.on("connect", () => {
      sock.write(line);
    });
    sock.on("data", (chunk: Buffer) => {
      buf += chunk.toString("utf8");
      if (buf.length > MAX_REPLY_CHARS) {
        clearTimeout(timer);
        sock.destroy();
        finish(null);
      }
    });
    sock.on("end", () => {
      clearTimeout(timer);
      try {
        const reply = JSON.parse(buf.trim());
        if (
          !reply ||
          typeof reply !== "object" ||
          reply.v !== 1 ||
          !Number.isInteger(reply.exit_code)
        ) {
          finish(null);
          return;
        }
        finish({ exitCode: reply.exit_code, stderr: typeof reply.stderr === "string" ? reply.stderr : "" });
      } catch {
        finish(null);
      }
    });
    sock.on("error", () => {
      clearTimeout(timer);
      finish(null);
    });
  });
}

// Sends the bare-flags argv that run_argv in gate.py reads to the resident
// gate's socket. This is the one path
// that reaches evaluate_call and returns a real allow or deny. There is no
// --mode on the wire, so the gate resolves the mode from config.yaml.
// --root is the socket's own directory, as gate_client.py derives it.
// --verify-timeout 4 keeps the gate's verify budget under this client's
// 5 s socket timeout. Exit 0 is the only allow. Exit 2 is a deny with the
// gate's reason. Any other answer, or none, is a deny that names the fix.
async function askGate(payload: Record<string, unknown>, env: NodeJS.ProcessEnv): Promise<Verdict> {
  const sockPath = gateSocketPath(env);
  const argv = ["--format", "opencode", "--root", path.dirname(sockPath), "--verify-timeout", "4"];
  const reply = await sendGateRequest(sockPath, argv, payload, GATE_TIMEOUT_MS, callerPaneFields(env));
  if (reply === null) return { allow: false, reason: UNREACHABLE_REASON };
  if (reply.exitCode === 0) return { allow: true, reason: "" };
  if (reply.exitCode === 2) return { allow: false, reason: reply.stderr || "openDaisugi gate: DENIED" };
  return { allow: false, reason: UNREACHABLE_REASON };
}

// Fire-and-forget: reaches `daisugi hook report` over the same socket,
// carrying this plugin's own callerPaneFields on the request so the
// resident gate can fan the report out to coppice or Herdr on THIS pane's
// behalf, since its own environment carries no pane identity at all. Never
// blocks the agent and never throws. When the gate answers nothing this
// code can read, the floor shows no state for this session, an honest
// unknown, never a guessed idle.
function reportState(
  state: string,
  sessionID: unknown,
  env: NodeJS.ProcessEnv,
  ask?: { id: string; tool: string; summary: string }
): void {
  try {
    const sockPath = gateSocketPath(env);
    if (!gateSocketTrustworthy(sockPath)) return;
    const ocSession = typeof sessionID === "string" ? sessionID : "";
    const event: Record<string, unknown> = {
      v: 1,
      ts: Date.now() / 1000,
      session_id: env.OPENDAISUGI_SESSION_ID || ocSession || `opencode-${process.pid}`,
      harness: "opencode",
      state,
      source: "headless",
      detail: ask ? ask.summary.slice(0, DETAIL_MAX) : "",
    };
    if (ocSession) event.harness_session_id = ocSession;
    if (ask) {
      event.ask = {
        id: ask.id,
        tool: ask.tool,
        summary: ask.summary.slice(0, DETAIL_MAX),
        deadline: Date.now() / 1000 + ASK_DEADLINE_S,
      };
    }
    const argv = ["hook", "report"];
    if (env.COPPICE_PANE) argv.push("--pane", env.COPPICE_PANE);
    const req = JSON.stringify({
      v: 1,
      argv,
      stdin_b64: Buffer.from(JSON.stringify(event)).toString("base64"),
      ...callerPaneFields(env),
    }) + "\n";
    const sock = net.createConnection(sockPath);
    const timer = setTimeout(() => sock.destroy(), REPORT_TIMEOUT_MS);
    sock.on("connect", () => sock.end(req));
    sock.on("data", () => {});
    sock.on("close", () => clearTimeout(timer));
    sock.on("error", () => clearTimeout(timer));
  } catch {
    // A reporting failure never touches the agent.
  }
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

export default (async ({ directory }) => {
  // Sessions that a task tool call starts inside this server. Their idle is
  // not the pane's idle, so it is not reported.
  const children = new Set<string>();
  return {
    "tool.execute.before": async (input, output) => {
      let verdict: Verdict;
      try {
        const args = output ? output.args : undefined;
        const payload: Record<string, unknown> = {
          tool_name: gateToolName(input?.tool),
          tool_input: args ?? {},
          session_id: process.env.OPENDAISUGI_SESSION_ID || str(input?.sessionID),
          cwd: callCwd(directory, input?.tool, args),
          tool_use_id: str(input?.callID),
        };
        verdict = await askGate(payload, process.env);
      } catch {
        verdict = { allow: false, reason: UNREACHABLE_REASON };
      }
      if (!verdict.allow) throw new Error(verdict.reason);
    },
    // Upstream reports that this hook does not fire today, as PINS.md
    // records. It is kept for the day it does. It reports blocked and never writes
    // output.status: the plugin watches OpenCode's own prompt and does not
    // answer it. The input is typed any, since nothing calls it now.
    "permission.ask": async (input: any) => {
      try {
        reportState("blocked", input?.sessionID, process.env, {
          id: str(input?.id),
          tool: str(input?.type),
          summary: str(input?.title),
        });
      } catch {
        // never throw into OpenCode
      }
    },
    event: async ({ event }) => {
      try {
        const ev = event as { type?: string; properties?: any };
        const p = ev?.properties ?? {};
        if (ev?.type === "session.created" && p.info?.parentID && typeof p.info.id === "string") {
          children.add(p.info.id);
        } else if (ev?.type === "session.idle") {
          if (!children.has(str(p.sessionID))) reportState("idle", p.sessionID, process.env);
        } else if (ev?.type === "permission.asked") {
          const patterns = Array.isArray(p.patterns) ? p.patterns.filter((x: unknown) => typeof x === "string") : [];
          reportState("blocked", p.sessionID, process.env, {
            id: str(p.id),
            tool: str(p.permission),
            summary: patterns.join(", "),
          });
        }
      } catch {
        // never throw into OpenCode
      }
    },
  };
}) satisfies Plugin;
