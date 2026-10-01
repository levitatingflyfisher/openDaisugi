// openDaisugi OpenClaw plugin: passive capture via before_tool_call.
// Spawns `daisugi hook record --format openclaw`, feeds it the tool call as
// JSON on stdin, and never blocks (passive). The block/requireApproval path
// is reserved for verified-pathway enforcement in a later release.
import { spawn } from "node:child_process";
import { definePluginEntry } from "openclaw/plugin-sdk/plugin-entry";

const RECORD_TIMEOUT_MS = 5000;

function record(event) {
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
      p.stdin.write(JSON.stringify({
        tool: event.toolName,
        args: event.params,
        session_id: event.sessionId ?? "no-session",
      }));
      p.stdin.end();
    } catch { clearTimeout(timer); finish({}); }
  });
}

export default definePluginEntry({
  id: "opendaisugi",
  name: "openDaisugi capture",
  description: "Capture tool calls for distillation.",
  register(api) {
    api.on("before_tool_call", async (event) => {
      const decision = await record(event);
      return decision && decision.block ? decision : undefined;
    }, { priority: 10 });
  },
});
