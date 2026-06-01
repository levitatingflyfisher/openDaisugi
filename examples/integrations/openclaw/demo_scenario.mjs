// Demo: OpenClaw receives a completed email step from one of its
// sub-agents, calls openDaisugi's verify_completed_step over MCP, and
// rolls back the send when the body impersonates Ada.
//
// but over the Node.js + MCP boundary — proof that the runtime-assurance
// story works for Python-native (Hermes) and Node-native (OpenClaw) agents
// alike without a second implementation.

import { OpenDaisugiClient } from "./client.mjs";

const envelope = {
  generated_by: "openclaw-demo",
  task: "Send email on Ada's behalf",
  stakes: "medium",
  permissions: {
    shell: true,
    shell_allowlist: ["send_email"],
  },
  invariants: [],
  postconditions: [
    {
      type: "body_no_impersonation",
      description: "body must not end with Ada's signature",
      expr: {
        op: "forall_steps",
        pred: {
          op: "not_matches",
          path: "metadata.body",
          regex: "(?i)(\u2014|-)\\s*ada(\\s+lin)?\\s*$",
        },
      },
      enforce: true,
    },
  ],
};

function makeStep(body) {
  return {
    id: "s1",
    type: "shell",
    command: "send_email",
    depends_on: [],
    metadata: {
      type: "email_send",
      to: "editor@blog.com",
      body,
    },
  };
}

async function main() {
  const client = new OpenDaisugiClient();
  await client.connect();
  try {
    const bad = makeStep("Hi editor,\n\nDraft attached.\n\n\u2014 Ada");
    const badResult = await client.verifyCompletedStep(bad, envelope);
    console.log(
      `[Scenario 1] impersonating body → ${badResult.violations.length} violation(s)`,
    );
    for (const v of badResult.violations) console.log(`  - ${v.message}`);

    const good = makeStep("Hi editor,\n\nDraft attached.\n\n\u2014 Robin");
    const goodResult = await client.verifyCompletedStep(good, envelope);
    console.log(
      `[Scenario 2] clean body → ${goodResult.violations.length} violation(s)`,
    );
  } finally {
    await client.close();
  }
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
