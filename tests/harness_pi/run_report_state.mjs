// Test support: fires one reportState call and exits, the way a
// fire-and-forget lifecycle handler does.
// Usage: node --experimental-strip-types run_report_state.mjs <state>
import { reportState } from "../../src/opendaisugi/harness_pi/extension/index.ts";

reportState(process.argv[2]);
// Give the fire-and-forget socket write a moment to reach the OS before exit.
await new Promise((r) => setTimeout(r, 200));
console.log("done");
