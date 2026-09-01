// Test support: loads the gate plugin the way OpenCode's loader does and
// runs one step against it. Prints one JSON line on stdout.
// Usage: node --experimental-strip-types drive.mjs '<step JSON>'
//
// Steps:
//   {"do": "exports"}                   the module's distinct export values
//   {"do": "before", "tool", "args", "sessionID", "callID"}
//   {"do": "events", "events": [...]}   each one through the event hook
//   {"do": "permission.ask", "input": {...}}
import * as mod from "../../src/opendaisugi/harness_opencode/plugin/daisugi-gate.ts";

const step = JSON.parse(process.argv[2]);
const directory = process.env.DRIVE_DIRECTORY ?? "/proj";

if (step.do === "exports") {
  const values = [...new Set(Object.values(mod))];
  console.log(JSON.stringify({ count: values.length, types: values.map((v) => typeof v) }));
  process.exit(0);
}

// OpenCode calls each distinct exported function with the plugin input.
const hooks = [];
for (const fn of new Set(Object.values(mod))) hooks.push(await fn({ directory }, {}));
const h = hooks[0];

if (step.do === "before") {
  try {
    await h["tool.execute.before"](
      { tool: step.tool, sessionID: step.sessionID ?? "ses_1", callID: step.callID ?? "call_1" },
      { args: step.args }
    );
    console.log(JSON.stringify({ threw: false }));
  } catch (err) {
    console.log(JSON.stringify({ threw: true, message: String(err?.message ?? err) }));
  }
} else if (step.do === "events") {
  for (const event of step.events) await h.event({ event });
  await new Promise((r) => setTimeout(r, 300));
  console.log(JSON.stringify({ done: true }));
} else if (step.do === "permission.ask") {
  const output = {};
  await h["permission.ask"](step.input, output);
  await new Promise((r) => setTimeout(r, 300));
  console.log(JSON.stringify({ output }));
}
