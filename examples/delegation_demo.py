"""Skills-as-contracts demo (v0.11.0).

Runs two scenarios:

1. An orchestrator delegates to a narrow "echo" skill. Subsumption holds;
   the delegation is allowed.
2. The same orchestrator is asked to delegate to a wider "destroyer" skill
   that can invoke ``rm``. Z3 proves the delegation unsafe and returns a
   concrete counterexample command that falsifies the claim.

The point: the orchestrator never has to trust the skill on its word. The
contract is verified mechanically.
"""

from __future__ import annotations

from opendaisugi.contracts import Contract, verify_delegation
from opendaisugi.models import Envelope, Invariant, Permission


def run() -> int:
    orchestrator = Envelope(
        generated_by="orchestrator",
        task="run a mixed pipeline",
        permissions=Permission(shell=True, shell_allowlist=["echo", "ls", "pytest"]),
        invariants=[Invariant(
            type="never_destructive",
            description="never issue destructive shell commands",
            expr={"op": "forall_steps", "pred": {
                "op": "not_matches", "path": "command", "regex": r"^rm ",
            }},
        )],
    )

    echo_skill = Contract(
        contract_id="c_echo_skill",
        skill_id="robin/echoer",
        version="0.1.0",
        envelope=Envelope(
            generated_by="robin-lora-1.5b",
            task="print user-provided strings",
            permissions=Permission(shell=True, shell_allowlist=["echo"]),
        ),
        guarantees=["only calls echo"],
    )

    destroyer_skill = Contract(
        contract_id="c_destroyer",
        skill_id="third-party/shell-runner",
        version="0.2.1",
        envelope=Envelope(
            generated_by="shell-runner",
            task="run arbitrary shell",
            permissions=Permission(shell=True, shell_allowlist=["echo", "rm"]),
        ),
        guarantees=["runs shell commands"],
    )

    print("Scenario 1: orchestrator delegates to narrow echo skill")
    decision = verify_delegation(orchestrator, echo_skill)
    print(f"  allowed:       {decision.allowed}")
    print(f"  subsumption:   holds={decision.subsumption.holds}  "
          f"{decision.subsumption.duration_ms:.1f} ms")
    print(f"  reason:        {decision.reason}")
    print()

    print("Scenario 2: orchestrator delegates to wider destroyer skill")
    decision = verify_delegation(orchestrator, destroyer_skill)
    print(f"  allowed:       {decision.allowed}")
    print(f"  subsumption:   holds={decision.subsumption.holds}  "
          f"{decision.subsumption.duration_ms:.1f} ms")
    if decision.counterexample is not None:
        print("  counterexample:")
        print(f"    command:             {decision.counterexample.step.command!r}")
        print(f"    outer rule violated: {decision.counterexample.outer_violation}")
        print(f"    inner justification: {decision.counterexample.inner_justification}")
    print(f"  reason:        {decision.reason}")

    return 0 if decision.allowed is False else 1


if __name__ == "__main__":
    raise SystemExit(run())
