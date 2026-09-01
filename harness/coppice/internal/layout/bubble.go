package layout

// stateRank orders pane states from the one that needs a person most to the
// one that needs nobody. A state not in the table ranks below every one that
// is, and the empty state ranks last.
var stateRank = map[string]int{
	"blocked": 5,
	"working": 4,
	"idle":    3,
	"unknown": 2,
	"done":    1,
	"":        0,
}

// Worst returns the state that needs attention most. Nothing folds to "".
func Worst(states []string) string {
	worst := ""
	best := -1
	for _, s := range states {
		r, ok := stateRank[s]
		if !ok {
			r = 0
		}
		if r > best {
			best = r
			worst = s
		}
	}
	return worst
}

// TaskState folds the state of every pane in the task and of every child
// task, recursively, with Worst. paneState answers for one pane id. A task
// with no pane and no child is "". A child already folded on the way down
// is skipped, so a file whose children form a cycle still ends.
func TaskState(tk Task, tree *Tree, paneState func(string) string) string {
	return taskState(tk, tree, paneState, map[string]bool{tk.ID: true})
}

func taskState(tk Task, tree *Tree, paneState func(string) string, seen map[string]bool) string {
	var states []string
	for _, p := range tree.Panes() {
		if p.TaskID == tk.ID {
			states = append(states, paneState(p.ID))
		}
	}
	for _, cid := range tk.Children {
		if seen[cid] {
			continue
		}
		seen[cid] = true
		child, ok := tree.Task(cid)
		if !ok {
			continue
		}
		states = append(states, taskState(child, tree, paneState, seen))
	}
	return Worst(states)
}
