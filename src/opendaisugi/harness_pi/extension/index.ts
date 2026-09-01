// The daisugi gate extension for pi. Every tool_call asks the resident gate
// over its Unix socket, fail-closed. Facts about pi's wire formats are
// recorded in PINS.md next to this file. Re-read PINS.md, or re-fetch the two
// sources it names, before changing anything here.
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import net from "node:net";
import fs from "node:fs";
import path from "node:path";

interface Verdict {
  allow: boolean;
  reason: string;
}

const GATE_TIMEOUT_MS = 5000;
const REPORT_TIMEOUT_MS = 1000;
const UNREACHABLE_REASON = "openDaisugi gate unreachable: run daisugi start";

function gateSocketPath(env: NodeJS.ProcessEnv): string {
  if (env.OPENDAISUGI_GATE_SOCK) return env.OPENDAISUGI_GATE_SOCK;
  return path.join(env.HOME || "", ".opendaisugi", "gate", "gate.sock");
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
  timeoutMs: number
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
    // event handler in pi's own process.
    let line: string;
    let sock: net.Socket;
    try {
      line = JSON.stringify({
        v: 1,
        argv,
        stdin_b64: Buffer.from(JSON.stringify(payload)).toString("base64"),
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

// Connects to the same resident-gate socket gate_client.py uses and sends the
// bare-flags argv that opendaisugi.gate's own parser reads (run_argv in
// gate.py). It is not a `daisugi` CLI subcommand line. This is the one path
// that reaches evaluate_call and returns a real allow or deny.
//
// No --mode on the wire. A mode baked into this argv would let a file in this
// extension's own directory, which the agent can write, outrank the one flag
// an installed hook command carries and the agent cannot rewrite. With the
// flag absent the gate resolves the mode server-side from config.yaml, the
// same trust level every other unset-mode caller gets.
//
// --verify-timeout 4 keeps the gate's inner verify budget under this
// client's own 5 s socket timeout, so a slow verify returns its verdict
// instead of reading as an unreachable gate.
async function askGate(
  event: { toolName: string; input?: unknown },
  ctx: { cwd?: string },
  opts: { timeoutMs?: number; env?: NodeJS.ProcessEnv } = {}
): Promise<Verdict> {
  const env = opts.env || process.env;
  const timeoutMs = opts.timeoutMs ?? GATE_TIMEOUT_MS;
  // session_id names the envelope. Without it the gate consults the
  // default envelope, which is a deny when none is registered.
  const payload: Record<string, unknown> = {
    tool_name: event.toolName,
    tool_input: event.input ?? {},
    cwd: ctx.cwd || "",
  };
  if (env.OPENDAISUGI_SESSION_ID) payload.session_id = env.OPENDAISUGI_SESSION_ID;
  const sockPath = gateSocketPath(env);
  const argv = ["--format", "pi", "--root", path.dirname(sockPath), "--verify-timeout", "4"];
  const reply = await sendGateRequest(sockPath, argv, payload, timeoutMs);
  if (reply === null) return { allow: false, reason: UNREACHABLE_REASON };
  if (reply.exitCode === 0) return { allow: true, reason: "" };
  if (reply.exitCode === 2) return { allow: false, reason: reply.stderr || "denied" };
  // Any other exit code is not a verdict this extension recognizes. A
  // crashed gate or a bug fails closed rather than leaning either way.
  return { allow: false, reason: UNREACHABLE_REASON };
}

// Fire-and-forget: reaches `daisugi hook report` over the same socket, the
// {"argv":["hook","report"],...} dispatch gate_server.py serves. Never blocks
// the agent. Errors are swallowed, the same as the Python state report. When
// the gate answers something this code cannot parse, or nothing at all, this
// function still returns without throwing, and the floor shows no state for
// this pane. That is an honest unknown, never a guessed idle.
function reportState(sessionState: string, env: NodeJS.ProcessEnv = process.env): void {
  const sockPath = gateSocketPath(env);
  // session_id is the daisugi session id. The pane id is a separate field.
  // COPPICE_PANE names the pane, not the session, so it must not leak into
  // session_id.
  const sessionId = env.OPENDAISUGI_SESSION_ID || `pi-${process.pid}`;
  const event: Record<string, unknown> = {
    v: 1,
    ts: Date.now() / 1000,
    session_id: sessionId,
    harness: "pi",
    state: sessionState,
    source: "headless",
    detail: "",
  };
  if (env.COPPICE_PANE) event.pane = env.COPPICE_PANE;
  if (!gateSocketTrustworthy(sockPath)) return;
  try {
    const sock = net.createConnection(sockPath);
    const timer = setTimeout(() => sock.destroy(), REPORT_TIMEOUT_MS);
    sock.on("connect", () => {
      const req = {
        v: 1,
        argv: ["hook", "report"],
        stdin_b64: Buffer.from(JSON.stringify(event)).toString("base64"),
      };
      sock.end(JSON.stringify(req) + "\n");
    });
    sock.on("close", () => clearTimeout(timer));
    sock.on("error", () => clearTimeout(timer));
  } catch {
    // A reporting failure never touches the agent.
  }
}

export default function (pi: ExtensionAPI) {
  pi.on("tool_call", async (event: any, ctx: any) => {
    const verdict = await askGate(event, ctx);
    if (!verdict.allow) return { block: true, reason: verdict.reason };
    return undefined;
  });
  pi.on("session_start", () => reportState("idle"));
  pi.on("agent_start", () => reportState("working"));
  pi.on("agent_end", () => reportState("idle"));
  pi.on("agent_settled", () => reportState("idle"));
}

export { askGate, reportState, gateSocketPath, gateSocketTrustworthy };
