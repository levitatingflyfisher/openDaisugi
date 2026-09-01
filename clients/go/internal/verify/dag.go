package verify

import (
	"fmt"
	"strings"
)

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
		violations = append(violations, VStep("dag", dup, fmt.Sprintf("duplicate step id '%s' — step ids must be unique", dup)))
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
				violations = append(violations, VStep("dag", s.ID, fmt.Sprintf("Step '%s' depends on unknown step '%s'", s.ID, dep)))
			}
		}
	}
	if len(violations) > 0 {
		return violations
	}

	if cycleNodes := findCycle(plan); cycleNodes != nil {
		// Cycle violations have step=null (a plan-level structural defect,
		// not attributable to one step) — matches conformance.md.
		violations = append(violations, V("dag", "Plan contains a cycle: "+strings.Join(cycleNodes, " -> ")))
	}
	return violations
}

// findCycle is nx.find_cycle(g, orientation="original") over the graph
// dag._build_graph makes (a node per step, an edge dep -> step, in plan
// order): the nodes of the cycle it reports, or nil when there is none.
// It is networkx's own walk, so the cycle named is networkx's.
func findCycle(plan ActionPlan) []string {
	var nodes []string
	adj := map[string][]string{}
	hasEdge := map[[2]string]bool{}
	addNode := func(n string) {
		if _, ok := adj[n]; !ok {
			adj[n] = nil
			nodes = append(nodes, n)
		}
	}
	for _, s := range plan.Steps {
		addNode(s.ID)
	}
	for _, s := range plan.Steps {
		for _, dep := range s.DependsOn {
			addNode(dep)
			e := [2]string{dep, s.ID}
			if !hasEdge[e] {
				hasEdge[e] = true
				adj[dep] = append(adj[dep], s.ID)
			}
		}
	}
	explored := map[string]bool{}
	for _, start := range nodes {
		if explored[start] {
			continue
		}
		var edges [][2]string
		seen := map[string]bool{start: true}
		active := map[string]bool{start: true}
		prevHead, havePrev := "", false
		for _, e := range edgeDFS(adj, start) {
			tail, head := e[0], e[1]
			if explored[head] {
				continue
			}
			if havePrev && tail != prevHead {
				for {
					if len(edges) == 0 {
						edges = nil
						active = map[string]bool{tail: true}
						break
					}
					popped := edges[len(edges)-1]
					edges = edges[:len(edges)-1]
					delete(active, popped[1])
					if len(edges) > 0 && tail == edges[len(edges)-1][1] {
						break
					}
				}
			}
			edges = append(edges, e)
			if active[head] {
				final := head
				i := 0
				for i = range edges {
					if edges[i][0] == final {
						break
					}
				}
				var out []string
				for _, ce := range edges[i:] {
					out = append(out, ce[0])
				}
				return out
			}
			seen[head] = true
			active[head] = true
			prevHead, havePrev = head, true
		}
		for n := range seen {
			explored[n] = true
		}
	}
	return nil
}

// edgeDFS is nx.edge_dfs from one start node: every edge once, in the
// order a depth-first walk over the adjacency lists meets it.
func edgeDFS(adj map[string][]string, start string) [][2]string {
	var out [][2]string
	visitedEdges := map[[2]string]bool{}
	next := map[string]int{}
	visitedNodes := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		if !visitedNodes[cur] {
			visitedNodes[cur] = true
			next[cur] = 0
		}
		if next[cur] >= len(adj[cur]) {
			stack = stack[:len(stack)-1]
			continue
		}
		to := adj[cur][next[cur]]
		next[cur]++
		e := [2]string{cur, to}
		if !visitedEdges[e] {
			visitedEdges[e] = true
			stack = append(stack, to)
			out = append(out, e)
		}
	}
	return out
}
