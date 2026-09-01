// Test support: exercises askGate the way pi's own runtime calls it, with the
// real process.env and no test-only overrides. Prints the verdict as one JSON
// line on stdout.
// Usage: node --experimental-strip-types run_tool_call.mjs <toolName> <inputJSON> <cwd>
import { askGate } from "../../src/opendaisugi/harness_pi/extension/index.ts";

const [, , toolName, inputJson, cwd] = process.argv;
const verdict = await askGate({ toolName, input: JSON.parse(inputJson) }, { cwd });
console.log(JSON.stringify(verdict));
