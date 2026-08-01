/**
 * Port of dag.py's check_dag: duplicate ids -> missing deps -> cycle,
 * each tier short-circuiting the next. Cycle violations carry no `step`
 * (compares as step=null / "").
 */

import type { ActionPlan, Violation } from "./models.js";
import { violation } from "./models.js";

export function checkDag(plan: ActionPlan): Violation[] {
  const violations: Violation[] = [];

  const seen = new Set<string>();
  const dupes: string[] = [];
  for (const step of plan.steps) {
    if (seen.has(step.id) && !dupes.includes(step.id)) dupes.push(step.id);
    seen.add(step.id);
  }
  for (const dup of dupes) {
    violations.push(
      violation("dag", `duplicate step id '${dup}' — step ids must be unique`, { step: dup })
    );
  }
  if (violations.length > 0) return violations;

  const stepIds = new Set(plan.steps.map((s) => s.id));
  for (const step of plan.steps) {
    for (const dep of step.depends_on) {
      if (!stepIds.has(dep)) {
        violations.push(
          violation("dag", `Step '${step.id}' depends on unknown step '${dep}'`, {
            step: step.id,
            missing_dep: dep,
          })
        );
      }
    }
  }
  if (violations.length > 0) return violations;

  // Cycle detection via DFS coloring (white/gray/black).
  const adjacency = new Map<string, string[]>();
  for (const step of plan.steps) adjacency.set(step.id, []);
  for (const step of plan.steps) {
    for (const dep of step.depends_on) {
      adjacency.get(dep)!.push(step.id);
    }
  }
  const WHITE = 0,
    GRAY = 1,
    BLACK = 2;
  const color = new Map<string, number>();
  for (const step of plan.steps) color.set(step.id, WHITE);
  let hasCycle = false;

  function dfs(node: string): boolean {
    color.set(node, GRAY);
    for (const next of adjacency.get(node) ?? []) {
      const c = color.get(next);
      if (c === GRAY) return true;
      if (c === WHITE && dfs(next)) return true;
    }
    color.set(node, BLACK);
    return false;
  }

  for (const step of plan.steps) {
    if (color.get(step.id) === WHITE) {
      if (dfs(step.id)) {
        hasCycle = true;
        break;
      }
    }
  }

  if (hasCycle) {
    violations.push(violation("dag", "Plan contains a cycle", {}));
  }

  return violations;
}
