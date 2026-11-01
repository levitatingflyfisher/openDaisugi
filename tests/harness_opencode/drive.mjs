// Test support: loads the warden plugin the way OpenCode's loaders do and
// runs one step against it. Prints one JSON line on stdout.
// Usage: node --experimental-strip-types drive.mjs '<step JSON>'
//
// Steps:
//   {"do": "exports"}                   the module's exports and the default's shape
//   {"do": "before", "tool", "args", "sessionID", "callID"}      OpenCode 1 (server())
//   {"do": "events", "events": [...]}   each one through the v1 event hook
//   {"do": "permission.ask", "input": {...}}
//   {"do": "v2before", "event": {...}}  OpenCode 2 (setup(ctx)): one execute.before event
//   {"do": "v2events", "events": [...]} OpenCode 2: events on ctx.event.subscribe
import * as mod from "../../src/opendaisugi/harness_opencode/plugin/daisugi-warden.ts";

const step = JSON.parse(process.argv[2]);
const directory = process.env.DRIVE_DIRECTORY ?? "/proj";

if (step.do === "exports") {
  const def = mod.default;
  console.log(JSON.stringify({
    names: Object.keys(mod),
    id: def?.id,
    setup: typeof def?.setup,
    server: typeof def?.server,
  }));
  process.exit(0);
}

// OpenCode 2: setup(ctx) on a context with no tool hooks: {"do": "v2bare", "ctx": {...}}.
if (step.do === "v2bare") {
  try {
    const cleanup = await mod.default.setup({ location: { directory }, ...step.ctx });
    console.log(JSON.stringify({ threw: false, cleanup: typeof cleanup }));
  } catch (err) {
    console.log(JSON.stringify({ threw: true, message: String(err?.message ?? err) }));
  }
  process.exit(0);
}

// OpenCode 2: setup(ctx) with a fake context that keeps the hooks it is given.
if (step.do === "v2before" || step.do === "v2events") {
  const hooks = {};
  const events = step.events ?? [];
  const ctx = {
    location: { directory },
    tool: { hook: async (name, cb) => { hooks[name] = cb; } },
    event: {
      subscribe: ({ signal } = {}) => (async function* () {
        for (const ev of events) if (!signal?.aborted) yield ev;
      })(),
    },
  };
  const cleanup = await mod.default.setup(ctx);
  if (step.do === "v2before") {
    try {
      await hooks["execute.before"](step.event);
      console.log(JSON.stringify({ threw: false }));
    } catch (err) {
      console.log(JSON.stringify({ threw: true, message: String(err?.message ?? err) }));
    }
  } else {
    await new Promise((r) => setTimeout(r, 300));
    console.log(JSON.stringify({ done: true, cleanup: typeof cleanup }));
  }
  if (typeof cleanup === "function") cleanup();
  process.exit(0);
}

// OpenCode 1.18.29 and later call server() on an object entrypoint.
const h = await mod.default.server({ directory }, {});

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
