package gate

import (
	"strings"

	"daisugi-verify/internal/pyjson"
)

var patchHeaders = []string{"*** Add File:", "*** Update File:", "*** Delete File:", "*** Move to:"}

// parseApplyPatch is hook.parse_apply_patch; ok is false where Python
// returns None. One heredoc around the patch is unwrapped with the
// oracle's own regex (it has a backreference), run on pyre.
func parseApplyPatch(v any) ([]string, bool) {
	text, isStr := v.(string)
	if !isStr {
		return nil, false
	}
	body := pyStrip(text)
	if m := pyMatch("hook._PATCH_HEREDOC", body); m != nil {
		body = m[2]
	}
	lines := strings.Split(body, "\n")
	begin, end := -1, -1
	for i, line := range lines {
		s := pyStrip(line)
		if begin < 0 && s == "*** Begin Patch" {
			begin = i
		}
		if end < 0 && s == "*** End Patch" {
			end = i
		}
	}
	if begin < 0 || end < 0 || begin >= end {
		return nil, false
	}
	var paths []string
	for _, line := range lines[begin+1 : end] {
		for _, head := range patchHeaders {
			if strings.HasPrefix(line, head) {
				if p := pyStrip(line[len(head):]); p != "" {
					paths = append(paths, p)
				}
				break
			}
		}
	}
	return paths, len(paths) > 0
}

var tierRank = map[string]int{tierSilent: 0, tierUndoable: 1, tierPermanent: 2}

// decidePatch is gate._decide_patch: one file write per path the patch
// names, each placed from the call's working directory.
func (r *runner) decidePatch(p *pyjson.Object, toolName string, env *envelope, cwd string) *decision {
	defer r.enter()()
	var patchText any
	if inp, ok := toolInputOf(p).(*pyjson.Object); ok {
		patchText = inp.Value("patchText")
	}
	paths, ok := parseApplyPatch(patchText)
	if !ok {
		d := r.deny("apply_patch: the gate cannot read this patch, so it denies it. A patch " +
			"needs Begin Patch and End Patch lines and at least one file header.")
		d.ToolName = toolName
		return d
	}
	callCwd, isStr := p.Value("cwd").(string)
	if !isStr || !isabs(callCwd) {
		d := r.deny("apply_patch: the call names no absolute working directory, so the gate " +
			"cannot place the patch's paths. It denies the patch.")
		d.ToolName = toolName
		return d
	}
	var decisions []*decision
	for _, raw := range paths {
		path := normpath(join(callCwd, raw))
		rec := &record{ToolName: "Write", StepType: "file_write", Path: path}
		d := r.decideRecord(p, rec, toolName, env, cwd)
		if d.PaneRule {
			return d
		}
		decisions = append(decisions, d)
	}
	chosen := decisions[len(decisions)-1]
	best := -1
	for _, d := range decisions {
		if d.WouldDeny {
			rank, ok := tierRank[d.Tier]
			if !ok {
				rank = 2
			}
			if rank > best {
				best, chosen = rank, d
			}
		}
	}
	chosen.ToolName = toolName
	return chosen
}
