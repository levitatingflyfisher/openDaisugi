// openDaisugi OpenClaw plugin: passive capture via before_tool_call.
// Spawns `daisugi hook record --format openclaw`, feeds it the tool call as
// JSON on stdin, and never blocks (passive). The block/requireApproval path
// is reserved for verified-pathway enforcement in a later release.
//
// The hook signature is (event, ctx). The event carries toolName, params,
// toolCallId and runId. The session id is on ctx (ctx.sessionId), next to
// ctx.agentId and ctx.runId. Source:
// https://docs.openclaw.ai/plugins/hooks/tool-policy
import { spawn } from "node:child_process";
import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";

const RECORD_TIMEOUT_MS = 5000;

export function payloadFor(event, ctx) {
  return {
    tool: event.toolName,
    args: event.params,
    session_id: (ctx && ctx.sessionId) || event.sessionId || "no-session",
    tool_use_id: event.toolCallId || "",
    run_id: (ctx && ctx.runId) || event.runId || "",
  };
}

function record(event, ctx) {
  return new Promise((resolve) => {
    let out = "";
    let done = false;
    const finish = (v) => { if (!done) { done = true; resolve(v); } };
    const p = spawn("daisugi", ["hook", "record", "--format", "openclaw"], {
      stdio: ["pipe", "pipe", "ignore"],
    });
    // Fail open if daisugi stalls — never hang before_tool_call.
    const timer = setTimeout(() => { try { p.kill("SIGKILL"); } catch {} finish({}); }, RECORD_TIMEOUT_MS);
    p.stdout.on("data", (d) => (out += d));
    p.on("error", () => { clearTimeout(timer); finish({}); });
    p.on("close", () => {
      clearTimeout(timer);
      try { finish(JSON.parse(out || "{}")); } catch { finish({}); }
    });
    try {
      p.stdin.write(JSON.stringify(payloadFor(event, ctx)));
      p.stdin.end();
    } catch { clearTimeout(timer); finish({}); }
  });
}

export default definePluginEntry({
  id: "opendaisugi",
  name: "openDaisugi capture",
  description: "Capture tool calls for distillation.",
  register(api) {
    api.on("before_tool_call", async (event, ctx) => {
      const decision = await record(event, ctx);
      return decision && decision.block ? decision : undefined;
    }, { priority: 10 });
  },
});
