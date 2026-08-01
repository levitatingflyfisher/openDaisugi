// Package weave runs a WORKFLOW: a small directed graph of steps, each a sprig
// agent task, each gated by the envelope. The plan is DATA (plain JSON — a format
// the model already knows, so no novel-format prompt-tax; the research showed
// invented encodings can *reverse* their savings). The control flow — order,
// dependencies — is deterministic Go and spends ZERO model tokens; the model only
// fills each step's task. The grammar is also a fence: a workflow with a cycle or
// a dangling dependency will not parse, so it cannot run.
//
// This is Design C, and the harness home for the ADR-0008 idea (parameterized,
// callable, composable pathways). MVP: parse + topological execution.
package weave

import (
	"encoding/json"
	"fmt"

	sprig "github.com/opendaisugi/sprig"
)

// Step is one node: an id, the task the agent performs, and the ids it needs first.
type Step struct {
	ID    string   `json:"id"`
	Task  string   `json:"task"`
	Needs []string `json:"needs,omitempty"`
}

// Workflow is the whole graph.
type Workflow struct {
	Name  string `json:"name"`
	Steps []Step `json:"steps"`
}

// Result is one step's outcome.
type Result struct {
	StepID string
	Output string
	Err    error
}

// Parse reads a workflow from JSON and validates it: ids are unique, every need
// points at a real step, and the graph is acyclic. An invalid workflow is a
// parse error — it never reaches execution.
func Parse(data []byte) (*Workflow, error) {
	var wf Workflow
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("weave: bad JSON: %w", err)
	}
	seen := map[string]bool{}
	for _, s := range wf.Steps {
		if s.ID == "" {
			return nil, fmt.Errorf("weave: a step has no id")
		}
		if seen[s.ID] {
			return nil, fmt.Errorf("weave: duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
	}
	for _, s := range wf.Steps {
		for _, n := range s.Needs {
			if !seen[n] {
				return nil, fmt.Errorf("weave: step %q needs %q, which does not exist", s.ID, n)
			}
		}
	}
	if _, err := topoOrder(wf.Steps); err != nil {
		return nil, err
	}
	return &wf, nil
}

// Run executes the steps in dependency order, each on an agent built by newAgent
// (which wires the model + the envelope gate). Fail-stop: if a step errors, the
// run stops. Returns each step's result in execution order.
func (wf *Workflow) Run(newAgent func(Step) *sprig.Agent) ([]Result, error) {
	order, err := topoOrder(wf.Steps)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, step := range order {
		out, runErr := newAgent(step).Run(step.Task)
		results = append(results, Result{StepID: step.ID, Output: out, Err: runErr})
		if runErr != nil {
			break
		}
	}
	return results, nil
}

// topoOrder returns the steps in a dependency-respecting order (Kahn's
// algorithm), or an error if the graph has a cycle.
func topoOrder(steps []Step) ([]Step, error) {
	byID := map[string]Step{}
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for _, s := range steps {
		byID[s.ID] = s
		if _, ok := indeg[s.ID]; !ok {
			indeg[s.ID] = 0
		}
	}
	for _, s := range steps {
		for _, n := range s.Needs {
			indeg[s.ID]++
			dependents[n] = append(dependents[n], s.ID)
		}
	}
	// queue of ready steps, in original declaration order for determinism
	var queue []string
	for _, s := range steps {
		if indeg[s.ID] == 0 {
			queue = append(queue, s.ID)
		}
	}
	var order []Step
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, byID[id])
		for _, d := range dependents[id] {
			indeg[d]--
			if indeg[d] == 0 {
				queue = append(queue, d)
			}
		}
	}
	if len(order) != len(steps) {
		return nil, fmt.Errorf("weave: the workflow has a cycle")
	}
	return order, nil
}
