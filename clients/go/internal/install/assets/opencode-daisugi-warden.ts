// The daisugi warden plugin for OpenCode. Every tool call asks the resident
// warden over its Unix socket before it runs. OpenCode's tool.execute.before
// hook can only deny, by a throw, so this plugin can never grant anything.
// Facts about OpenCode's plugin loader and hook shapes are recorded in
// harness/coppice/internal/adapters/opencode/PINS.md.
//
// This file exports one value only, the default plugin definition. It
// carries both plugin APIs. OpenCode 2 reads `id` and calls `setup(ctx)`.
// OpenCode 1.18.29 and later call `server(input)` on an object entrypoint.
// OpenCode 2 does not run a v1 plugin: it logs one WARN and goes on with no
// warden at all, so a file that only one of them can load leaves the other
// with no warden. Keep every helper below private to this module.
//
// No warden mode lives in this file. The warden resolves audit or enforce
// itself from config.yaml, a file an agent in OpenCode cannot rewrite
// without the warden's own leave.
import type { Plugin } from "@opencode-ai/plugin";
import net from "node:net";
import fs from "node:fs";
import path from "node:path";

interface Verdict {
  allow: boolean;
  reason: string;
}

const WARDEN_TIMEOUT_MS = 5000;
const REPORT_TIMEOUT_MS = 1000;
const ASK_DEADLINE_S = 90;
const DETAIL_MAX = 200;
const UNREACHABLE_REASON = "openDaisugi warden unreachable: run `daisugi start`, or `daisugi warden serve`";
// A warden reply is one short JSON line. A longer reply is not a verdict.
const MAX_REPLY_CHARS = 1 << 20;

// OpenCode's built-in tool ids, mapped to the tool names the warden already
// classifies. Every other id, such as apply_patch, task, skill, todowrite
// or an MCP tool, goes to the warden as mcp__opencode__<id>. The warden treats
// that as an MCP call: denied unless the envelope's mcp_allowlist names
// opencode/<id>, and never an undoable ask.
const TOOL_NAMES: Record<string, string> = {
  bash: "Bash",
  shell: "Bash", // OpenCode 2's name for its bash tool
  read: "Read",
  write: "Write",
  edit: "Edit",
  glob: "Glob",
  grep: "Grep",
  webfetch: "WebFetch",
  websearch: "WebSearch",
};

function wardenToolName(id: unknown): string {
  if (typeof id !== "string" || id === "") return "";
  return Object.hasOwn(TOOL_NAMES, id) ? TOOL_NAMES[id] : `mcp__opencode__${id}`;
}

// The directory a call runs in. OpenCode's bash tool takes a workdir
// argument, and the warden must check the command from that directory.
function callCwd(directory: string, tool: unknown, args: any): string {
  const base = typeof directory === "string" ? directory : "";
  if ((tool !== "bash" && tool !== "shell") || args === null || typeof args !== "object" || !("workdir" in args)) {
    return base;
  }
  if (typeof args.workdir !== "string") throw new Error("workdir is not a string");
  if (base) return path.resolve(base, args.workdir);
  // With no session directory a relative workdir has no place to start.
  if (!path.isAbsolute(args.workdir)) throw new Error("relative workdir with no directory");
  return args.workdir;
}

// The data home, as opendaisugi.datahome picks it: OPENDAISUGI_HOME, else
// $XDG_DATA_HOME/opendaisugi when ~/.opendaisugi does not exist, else
// ~/.opendaisugi. The warden's socket sits under it, in warden/. "" when there
// is no HOME and no OPENDAISUGI_HOME.
function dataHome(env: NodeJS.ProcessEnv): string {
  const home = env.HOME && path.isAbsolute(env.HOME) ? env.HOME : "";
  const od = usable(env.OPENDAISUGI_HOME, home);
  if (od) return od;
  if (!home) return "";
  const dot = path.join(home, ".opendaisugi");
  const xdg = usable(env.XDG_DATA_HOME, home);
  if (xdg && !fs.existsSync(dot)) return path.join(xdg, "opendaisugi");
  return dot;
}

// A data home variable as opendaisugi.datahome reads it: a leading ~ or ~/
// is the home directory, and a value that is still not absolute is
// ignored (""), so the socket is never looked for relative to the cwd.
function usable(v: string | undefined, home: string): string {
  if (!v) return "";
  if ((v === "~" || v.startsWith("~/")) && !home) return "";
  if (v === "~") v = home;
  else if (v.startsWith("~/")) v = home + v.slice(1);
  return path.isAbsolute(v) ? path.normalize(v) : "";
}

function wardenSocketPath(env: NodeJS.ProcessEnv): string {
  if (env.OPENDAISUGI_WARDEN_SOCK) return env.OPENDAISUGI_WARDEN_SOCK;
  const home = dataHome(env);
  if (!home) return "";
  return path.join(home, "warden", "warden.sock");
}

// This process's own coppice/Herdr pane identity, read from ITS OWN
// environment: OpenCode runs as the actual pane process (or a descendant
// of it), so its environment IS the caller's identity. Sent as explicit
// request fields on every call to the resident warden, mirroring
// warden_client.py's caller_pane_fields: the resident warden is a separate
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
  // The resident warden guards it as it guards the default one. Only an
  // absolute path is sent, since coppice always sets one.
  const dataDir = env.COPPICE_DATA_DIR;
  if (dataDir && path.isAbsolute(dataDir)) fields.coppice_data_dir = dataDir;
  return fields;
}

// Mirrors warden_client.py's _socket_is_trustworthy: the path entry must be a
// real socket, owned by this uid, mode 0600. lstat, never stat: stat follows
// symlinks, so a rogue process could plant a symlink to a socket it controls
// and have this trust it. Without this check any local process that wins the
// path answers {"exit_code":0} and every tool call is allowed.
function wardenSocketTrustworthy(p: string): boolean {
  try {
    const st = fs.lstatSync(p);
    return st.isSocket() && st.uid === process.getuid!() && (st.mode & 0o777) === 0o600;
  } catch {
    return false;
  }
}

interface WardenReply {
  exitCode: number;
  stderr: string;
}

function sendWardenRequest(
  sockPath: string,
  argv: string[],
  payload: unknown,
  timeoutMs: number,
  extraFields: Record<string, unknown> = {}
): Promise<WardenReply | null> {
  return new Promise((resolve) => {
    if (!wardenSocketTrustworthy(sockPath)) {
      resolve(null);
      return;
    }
    let settled = false;
    const finish = (v: WardenReply | null) => {
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

// Sends the bare-flags argv that run_argv in warden.py reads to the resident
// warden's socket. This is the one path
// that reaches evaluate_call and returns a real allow or deny. There is no
// --mode on the wire, so the warden resolves the mode from config.yaml.
// No --root: the server's root is fixed when it starts, and it denies a
// request that names one (FN-R-21).
// --verify-timeout 4 keeps the warden's verify budget under this client's
// 5 s socket timeout. Exit 0 is the only allow. Exit 2 is a deny with the
// warden's reason. Any other answer, or none, is a deny that names the fix.
async function askWarden(payload: Record<string, unknown>, env: NodeJS.ProcessEnv): Promise<Verdict> {
  const sockPath = wardenSocketPath(env);
  const argv = ["--format", "opencode", "--verify-timeout", "4"];
  const reply = await sendWardenRequest(sockPath, argv, payload, WARDEN_TIMEOUT_MS, callerPaneFields(env));
  if (reply === null) return { allow: false, reason: UNREACHABLE_REASON };
  if (reply.exitCode === 0) return { allow: true, reason: "" };
  if (reply.exitCode === 2) return { allow: false, reason: reply.stderr || "openDaisugi warden: DENIED" };
  return { allow: false, reason: UNREACHABLE_REASON };
}

// Fire-and-forget: reaches `daisugi hook report` over the same socket,
// carrying this plugin's own callerPaneFields on the request so the
// resident warden can fan the report out to coppice or Herdr on THIS pane's
// behalf, since its own environment carries no pane identity at all. Never
// blocks the agent and never throws. When the warden answers nothing this
// code can read, the floor shows no state for this session, an honest
// unknown, never a guessed idle.
function reportState(
  state: string,
  sessionID: unknown,
  env: NodeJS.ProcessEnv,
  ask?: { id: string; tool: string; summary: string }
): void {
  try {
    const sockPath = wardenSocketPath(env);
    if (!wardenSocketTrustworthy(sockPath)) return;
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

// Asks the warden about one tool call and throws its reason on a deny. Both
// plugin APIs call it; a throw is the only deny either one offers.
async function guardCall(directory: string, tool: unknown, args: any, sessionID: unknown, callID: unknown): Promise<void> {
  let verdict: Verdict;
  try {
    const payload: Record<string, unknown> = {
      tool_name: wardenToolName(tool),
      tool_input: args ?? {},
      session_id: process.env.OPENDAISUGI_SESSION_ID || str(sessionID),
      cwd: callCwd(directory, tool, args),
      tool_use_id: str(callID),
    };
    verdict = await askWarden(payload, process.env);
  } catch {
    verdict = { allow: false, reason: UNREACHABLE_REASON };
  }
  if (!verdict.allow) throw new Error(verdict.reason);
}

// The OpenCode 1 plugin: a function that returns its hooks by name.
const serverV1 = (async ({ directory }) => {
  // Sessions that a task tool call starts inside this server. Their idle is
  // not the pane's idle, so it is not reported.
  const children = new Set<string>();
  return {
    "tool.execute.before": async (input, output) => {
      await guardCall(directory, input?.tool, output ? output.args : undefined, input?.sessionID, input?.callID);
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

// The OpenCode 2 plugin. Live on 2.2.6: the execute.before event is
// {tool, sessionID, agent, messageID, id, input}, the bash tool is "shell",
// read takes {path}, and a throw from the hook denies the call. A turn that
// ends is session.execution.succeeded; there is no session.idle. Not seen
// live yet: permission.asked and child sessions, so neither is reported.
async function setupV2(ctx: any) {
  const directory = str(ctx?.location?.directory);
  if (typeof ctx?.tool?.hook !== "function") {
    const version = str(ctx?.app?.version);
    // OpenCode 1's second loader may call setup with no tool domain;
    // server() is the warden there, so this load stays quiet.
    if (!ctx?.tool && (version === "" || version.startsWith("1."))) return;
    // Anywhere else no hook means no warden. The throw is the one way to
    // be heard: OpenCode logs it as one WARN line and runs on.
    throw new Error(
      `daisugi-warden: OpenCode ${version || "(unknown version)"} has no tool.hook, ` +
        "so the warden is OFF and every tool call runs unchecked. Update openDaisugi, or remove the plugin."
    );
  }
  await ctx.tool.hook("execute.before", async (event: any) => {
    await guardCall(directory, event?.tool, event?.input, event?.sessionID, event?.id);
  });
  const controller = new AbortController();
  void (async () => {
    try {
      for await (const ev of ctx.event.subscribe({ signal: controller.signal })) {
        try {
          if (ev?.type === "session.execution.succeeded") reportState("idle", ev?.data?.sessionID, process.env);
        } catch {
          // never throw into OpenCode
        }
      }
    } catch {
      // the stream ends when the plugin unloads
    }
  })();
  return () => controller.abort();
}

export default { id: "daisugi-warden", setup: setupV2, server: serverV1 };
