"""Safe subagent from a local model — runnable demo.

Shows the turnkey pattern for "cheap local-model subagents that can't act outside
a verified scope":

  1. A parent holds broad authority (its envelope).
  2. A subagent is published as a Contract with a NARROW envelope.
  3. SafeSubagent.create proves the subagent's scope is subsumed by the parent
     (fail-closed) — an over-reaching subagent is refused at creation.
  4. Every plan the subagent runs is verified against its scope, dry-run by default.
  5. A local Tier-1 model (free-ish tokens) is the subagent's configured brain —
     your loop proposes plans with it; SafeSubagent verifies them. Wire one via
     tier1=... (commented below).

This is PLAN-LEVEL runtime assurance, not an OS sandbox. Run:  python run_demo.py
"""

import asyncio

from opendaisugi import (
    ActionPlan,
    Contract,
    Envelope,
    Permission,
    SafeSubagent,
    ShellStep,
)
from opendaisugi.subagent import DelegationDenied

# A local model as the subagent's brain (cheap/free tokens). Uncomment to wire:
# from opendaisugi.tier1 import LiteLLMTier1Provider
# LOCAL = LiteLLMTier1Provider("qwen2.5-1.5b", base_url="http://localhost:8080/v1")
LOCAL = None


async def main() -> None:
    # 1. Parent authority: may run a handful of read-only shell tools.
    parent = Envelope(
        generated_by="parent",
        task="supervise read-only inspection subagents",
        permissions=Permission(shell=True, shell_allowlist=["ls", "cat", "grep", "echo"]),
    )

    # 2. A narrow subagent: only `ls` and `cat`.
    inspector = Contract(
        contract_id="inspector-v1",
        skill_id="file-inspector",
        envelope=Envelope(
            generated_by="subagent",
            task="inspect files read-only",
            permissions=Permission(shell=True, shell_allowlist=["ls", "cat"]),
        ),
    )

    # 3. Mint the subagent — proven subsumed by the parent (else DelegationDenied).
    sub = SafeSubagent.create(parent_envelope=parent, contract=inspector, tier1=LOCAL)
    print(f"subagent created: scope={sub.envelope.permissions.shell_allowlist} "
          f"(delegation allowed: {sub.decision.allowed})")

    # 4a. An in-scope plan verifies and dry-runs cleanly.
    ok_plan = ActionPlan(
        source="file-inspector", task="list the logs",
        steps=[ShellStep(id="s1", command="ls /var/log")],
    )
    session = await sub.run(ok_plan)  # dry-run by default
    print(f"in-scope plan: status={session.status.name}; "
          f"step said: {session.steps[0].stdout!r}")

    # 4b. An out-of-scope plan is rejected BEFORE execution.
    bad_plan = ActionPlan(
        source="file-inspector", task="delete the logs",
        steps=[ShellStep(id="s1", command="rm -rf /var/log")],
    )
    result = sub.verify(bad_plan)
    print(f"out-of-scope plan: verify ok={result.ok} "
          f"(violations: {[v.message for v in result.violations][:1]})")

    # 5. An over-reaching subagent can't even be created.
    try:
        SafeSubagent.create(
            parent_envelope=parent,
            contract=Contract(
                contract_id="rm-bot", skill_id="deleter",
                envelope=Envelope(
                    generated_by="subagent", task="delete things",
                    permissions=Permission(shell=True, shell_allowlist=["ls", "rm"]),
                ),
            ),
        )
    except DelegationDenied as exc:
        print(f"over-reaching subagent refused: {exc.reason}")


if __name__ == "__main__":
    asyncio.run(main())
