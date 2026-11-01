// Test support: loads the OpenClaw plugin source with its one SDK import
// replaced by a stub, then prints payloadFor(event, ctx) as JSON.
// Usage: node run_before_tool_call.mjs <plugin path> '<event JSON>' '<ctx JSON>'
import { readFileSync } from "node:fs";

const [, , pluginPath, eventJson, ctxJson] = process.argv;
const source = readFileSync(pluginPath, "utf8").replace(
  /^import \{ definePluginEntry \} from "openclaw\/plugin-sdk\/plugin-entry";$/m,
  "const definePluginEntry = (x) => x;"
);
const url = "data:text/javascript;base64," + Buffer.from(source).toString("base64");
const mod = await import(url);
console.log(JSON.stringify(mod.payloadFor(JSON.parse(eventJson), JSON.parse(ctxJson))));
