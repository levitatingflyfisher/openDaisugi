// OpenClaw × openDaisugi MCP client.
//
// Spawns `python -m opendaisugi.cli mcp-serve` as a subprocess, speaks
// MCP over stdio, and exposes the three tools OpenClaw actually needs:
// envelope_for, verify_plan, verify_completed_step.
//
// Keep this file thin — all the runtime-assurance logic lives on the
// Python side. The client's job is translation and nothing more.

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

export class OpenDaisugiClient {
  constructor() {
    this.client = null;
  }

  async connect({ command = "daisugi", args = ["mcp", "serve"] } = {}) {
    const transport = new StdioClientTransport({ command, args });
    this.client = new Client(
      { name: "openclaw-demo", version: "0.1.0" },
      { capabilities: {} },
    );
    await this.client.connect(transport);
    return this.client;
  }

  async envelopeFor(task, { stakes = "medium", context = null } = {}) {
    const res = await this.client.callTool({
      name: "envelope_for",
      arguments: { task, stakes, ...(context ? { context } : {}) },
    });
    return res.structuredContent ?? res.content;
  }

  async verifyPlan(plan, envelope) {
    const res = await this.client.callTool({
      name: "verify_plan",
      arguments: { plan, envelope },
    });
    return res.structuredContent ?? res.content;
  }

  async verifyCompletedStep(step, envelope) {
    const res = await this.client.callTool({
      name: "verify_completed_step",
      arguments: { step, envelope },
    });
    return res.structuredContent ?? res.content;
  }

  async close() {
    if (this.client) await this.client.close();
  }
}
