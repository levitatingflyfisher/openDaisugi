package verify

// CheckDAG ports dag.check_dag: duplicate step ids, missing dependencies,
// then cycle detection — each tier short-circuits the next.
func CheckDAG(plan ActionPlan) []Violation {
	var violations []Violation

	seen := map[string]bool{}
	dupeSeen := map[string]bool{}
	var dupes []string
	for _, s := range plan.Steps {
		if seen[s.ID] && !dupeSeen[s.ID] {
			dupes = append(dupes, s.ID)
			dupeSeen[s.ID] = true
		}
		seen[s.ID] = true
	}
	for _, dup := range dupes {
		violations = append(violations, VStep("dag", dup))
	}
	if len(violations) > 0 {
		return violations
	}

	stepIDs := map[string]bool{}
	for _, s := range plan.Steps {
		stepIDs[s.ID] = true
	}
	for _, s := range plan.Steps {
		for _, dep := range s.DependsOn {
			if !stepIDs[dep] {
				violations = append(violations, VStep("dag", s.ID))
			}
		}
	}
	if len(violations) > 0 {
		return violations
	}

	if cycleNodes := findCycle(plan); cycleNodes != nil {
		// Cycle violations have step=null (a plan-level structural defect,
		// not attributable to one step) — matches conformance.md.
		violations = append(violations, V("dag"))
	}
	return violations
}

// findCycle returns the nodes of one cycle (DFS-based, matching
// nx.find_cycle's "orientation=original" edge direction: dep -> step), or
// nil if the graph is acyclic. Only existence/non-existence is normative
// (the wire verdict for a cycle carries step=null), so the exact node
// ordering doesn't need to match networkx's.
func findCycle(plan ActionPlan) []string {
	adj := map[string][]string{}
	for _, s := range plan.Steps {
		for _, dep := range s.DependsOn {
			adj[dep] = append(adj[dep], s.ID)
		}
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	for _, s := range plan.Steps {
		color[s.ID] = white
	}
	var cyclic bool
	var visit func(id string)
	visit = func(id string) {
		if cyclic {
			return
		}
		color[id] = gray
		for _, next := range adj[id] {
			if cyclic {
				return
			}
			switch color[next] {
			case gray:
				cyclic = true
				return
			case white:
				visit(next)
			}
		}
		color[id] = black
	}
	for _, s := range plan.Steps {
		if cyclic {
			break
		}
		if color[s.ID] == white {
			visit(s.ID)
		}
	}
	if cyclic {
		return []string{"cycle"}
	}
	return nil
}
