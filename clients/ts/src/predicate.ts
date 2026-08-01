/**
 * Port of predicate.py — the expression algebra. Kept as tagged plain
 * objects (not classes) since we only need to parse/inspect, not
 * pydantic-validate.
 */

export type Expression =
  | { op: "equals"; path: string; value: any }
  | { op: "not_equals"; path: string; value: any }
  | { op: "in_set"; path: string; values: any[] }
  | { op: "not_in_set"; path: string; values: any[] }
  | { op: "matches"; path: string; regex: string }
  | { op: "not_matches"; path: string; regex: string }
  | { op: "numeric_range"; path: string; min: number; max: number }
  | { op: "length_range"; path: string; min: number; max: number | null }
  | { op: "exists"; path: string }
  | { op: "is_empty"; path: string }
  | { op: "and"; children: Expression[] }
  | { op: "or"; children: Expression[] }
  | { op: "not"; child: Expression }
  | { op: "implies"; a: Expression; b: Expression }
  | { op: "forall_steps"; pred: Expression }
  | { op: "exists_step"; pred: Expression }
  | { op: "forall_outputs"; pred: Expression }
  | { op: "depends_on"; step_id_a: string; step_id_b: string }
  | { op: "before"; step_id_a: string; step_id_b: string }
  | { op: "alias"; name: string; args: Record<string, any> }
  | { op: "llm_check"; rule: string };

/** parse_expression — a raw dict is already shaped like an Expression once
 * it carries an `op` discriminator; this just narrows/normalizes defaults. */
export function parseExpression(data: any): Expression {
  if (data === null || typeof data !== "object" || typeof data.op !== "string") {
    throw new Error(`invalid predicate expression: ${JSON.stringify(data)}`);
  }
  switch (data.op) {
    case "in_set":
    case "not_in_set":
      return { op: data.op, path: data.path, values: data.values ?? [] };
    case "length_range":
      return { op: data.op, path: data.path, min: data.min ?? 0, max: data.max ?? null };
    case "alias":
      return { op: "alias", name: data.name, args: data.args ?? {} };
    default:
      return data as Expression;
  }
}

export function isAliasRef(e: Expression): e is { op: "alias"; name: string; args: Record<string, any> } {
  return e.op === "alias";
}
