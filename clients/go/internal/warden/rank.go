package warden

import (
	"strings"

	"daisugi-verify/internal/verify"
)

// rankRefusal is rank_rule.REFUSAL.
const rankRefusal = "only the owner records a ranking vote. Record it yourself."

// treeRefusal is tree_rule.REFUSAL.
const treeRefusal = "only the operator or the starter changes the delegation tree. Run it yourself."

// wardenChangeRefusal is owner_rule.REFUSAL.
const wardenChangeRefusal = "only the operator changes how the warden enforces. Run it yourself."

// ownerCommands is owner_rule.COMMANDS: every top-level command of the CLI.
var ownerCommands = setOf("agent-profiles", "batch", "bench", "config", "conformance", "coppice", "dashboard",
	"distill-repeats", "gardener", "warden", "router", "router report", "generate-envelope", "graft",
	"help", "hook", "install", "journal", "lora", "mcp", "metrics", "models", "modules", "onboard",
	"orchestrate", "pack", "pando", "pathways", "rank", "recipe", "registry", "release", "route", "router", "run", "setup",
	"start", "status", "tend", "tiers", "tree", "verify", "viz", "voice", "weave")

// ownerSubcommands is owner_rule.SUBCOMMANDS.
var ownerSubcommands = map[string]map[string]bool{
	"warden": setOf("arm", "audit", "check", "disarm", "init", "proposals", "register", "replay",
		"report", "schemas", "serve", "settings", "status"),
	"graft":          setOf("install", "remove", "status"),
	"pack":           setOf("bundle", "install", "list", "remove", "run", "status"),
	"router":         setOf("label", "status", "stop"),
	"rank":           setOf("choose", "fit", "judge", "judges", "pivot", "queue", "record", "show"),
	"tree":           setOf("adopt", "answer", "check", "end", "prune-branches", "root", "run", "spawn", "status"),
	"agent-profiles": setOf("bundle", "list"),
	"recipe":         setOf("apply", "list", "revert", "show"),
}

// A verb is its command and subcommand words joined by a space.
var (
	rankVerbs = setOf("rank record")
	// pivotVerbs is owner_rule.PIVOT_VERBS: a hit only with --apply.
	pivotVerbs = setOf("rank pivot")
	treeVerbs  = setOf("warden register", "warden init", "start", "tree root", "tree spawn", "tree run", "tree end",
		"tree adopt", "tree answer", "tree prune-branches", "agent-profiles bundle", "agent-profiles list")
	wardenVerbs = setOf("warden disarm", "warden arm", "warden serve", "install", "graft install",
		"graft remove", "recipe apply", "recipe revert")
	labelVerbs = setOf("router label")
	packVerbs  = setOf("pack install", "pack remove", "pack bundle")
)

// labelRefusal is owner_rule.LABEL_REFUSAL.
const labelRefusal = "only the operator labels a task's outcome. Label it yourself."

// packRefusal is owner_rule.PACK_REFUSAL.
const packRefusal = "only the operator installs, removes or bundles a pack. Run it yourself."

func setOf(ws ...string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// ownerWord is owner_rule._WORD: the ASCII characters a word is made of.
func ownerWord(c rune) bool {
	return c < 0x80 && (('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') ||
		strings.ContainsRune("_./:=+@%,-", c))
}

// ownerWords is owner_rule.words: backslashes and quotes dropped, then the
// runs of word characters. Any other character, non-ASCII included, ends
// a word.
func ownerWords(text string) []string {
	text = strings.ReplaceAll(text, "\\\n", "")
	text = strings.NewReplacer("\\", "", "'", "", "\"", "").Replace(text)
	var out []string
	var cur strings.Builder
	for _, c := range text {
		if ownerWord(c) {
			cur.WriteRune(c)
		} else if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ownerHead is owner_rule.is_head.
func ownerHead(w string) bool {
	last := w[strings.LastIndex(w, "/")+1:]
	return last == "daisugi" || last == "daisugi-py" || last == "opendaisugi" ||
		strings.HasPrefix(last, "opendaisugi.")
}

// scanWords is owner_rule.scan_words: a head word, then each word of a
// verb, in that order.
func scanWords(text string, verbs map[string]bool) bool {
	ws := ownerWords(text)
	for verb := range verbs {
		parts := strings.Split(verb, " ")
		want := 0
		for _, w := range ws {
			if want == 0 {
				if ownerHead(w) {
					want = 1
				}
			} else if w == parts[want-1] {
				want++
				if want > len(parts) {
					return true
				}
			}
		}
	}
	return false
}

// commandPath is owner_rule.command_path, joined by a space; "" when the
// arguments name no command.
func commandPath(args []string) string {
	var pos []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		}
	}
	for i, w := range pos {
		if ownerCommands[w] {
			if subs, ok := ownerSubcommands[w]; ok {
				for _, x := range pos[i+1:] {
					if subs[x] {
						return w + " " + x
					}
				}
			}
			return w
		}
	}
	return ""
}

// hasNonWord is any(ch not in _WORD for ch in tok).
func hasNonWord(tok string) bool {
	for _, c := range tok {
		if !ownerWord(c) {
			return true
		}
	}
	return false
}

// simpleCommands is owner_rule._simple_commands: the simple commands of a
// shell line, or ok=false when it does not decompose or the parser raises.
func (r *runner) simpleCommands(command string) (cmds []string, ok bool) {
	defer r.enter()()
	if exc := catch(func() {
		d, err := r.decompose(command)
		if err != nil {
			panic(err)
		}
		if d.OK && len(d.Commands) > 0 {
			cmds, ok = d.Commands, true
		}
	}); exc != nil {
		return nil, false
	}
	return cmds, ok
}

// simpleHit is owner_rule._simple_hit.
func simpleHit(simple string, verbs map[string]bool) bool {
	tokens, err := verify.ShlexSplit(simple)
	if err != nil {
		return scanWords(simple, verbs)
	}
	for i, tok := range tokens {
		if ownerHead(tok) {
			if p := commandPath(tokens[i+1:]); p != "" && verbs[p] {
				return true
			}
		} else if hasNonWord(tok) && scanWords(tok, verbs) {
			return true
		}
	}
	// owner_rule._SHLEX_GAPS: forms shlex splits differently from the
	// shell are also read by word order.
	for _, form := range []string{"\\\n", "$'", "$\""} {
		if strings.Contains(simple, form) {
			return scanWords(simple, verbs)
		}
	}
	return false
}

// runsVerb is owner_rule.runs_verb.
func (r *runner) runsVerb(command string, verbs map[string]bool) bool {
	defer r.enter()()
	simples, ok := r.simpleCommands(command)
	if !ok {
		return scanWords(command, verbs)
	}
	for _, s := range simples {
		if simpleHit(s, verbs) {
			return true
		}
	}
	return false
}

// shellVerbHit is owner_rule.shell_hit: a shell call whose command is a
// string that runs one of verbs. Any error is a hit.
func (r *runner) shellVerbHit(rec *record, verbs map[string]bool) (hit bool) {
	defer r.enter()()
	if exc := catch(func() {
		if rec == nil || rec.StepType != "shell" {
			return
		}
		if c, ok := rec.CommandRaw.(string); ok {
			hit = r.runsVerb(c, verbs)
		}
	}); exc != nil {
		return true
	}
	return hit
}

// rankRecordHit is rank_rule.rank_record_hit.
func (r *runner) rankRecordHit(rec *record) bool {
	defer r.enter()()
	if r.shellVerbHit(rec, rankVerbs) {
		return true
	}
	return r.pivotApplyHit(rec)
}

// pivotApplyHit is the second half of rank_rule.names_rank_record: a shell
// line that runs `daisugi rank pivot` and has the word --apply. Any error
// is a hit.
func (r *runner) pivotApplyHit(rec *record) (hit bool) {
	defer r.enter()()
	if exc := catch(func() {
		if rec == nil || rec.StepType != "shell" {
			return
		}
		c, ok := rec.CommandRaw.(string)
		if !ok || !r.runsVerb(c, pivotVerbs) {
			return
		}
		for _, w := range ownerWords(c) {
			if w == "--apply" {
				hit = true
			}
		}
	}); exc != nil {
		return true
	}
	return hit
}

// treeWriteHit is tree_rule.tree_write_hit.
func (r *runner) treeWriteHit(rec *record) bool {
	defer r.enter()()
	return r.shellVerbHit(rec, treeVerbs)
}

// wardenChangeHit is owner_rule.warden_change_hit.
func (r *runner) wardenChangeHit(rec *record) bool {
	defer r.enter()()
	return r.shellVerbHit(rec, wardenVerbs)
}

// labelHit is owner_rule.label_hit.
func (r *runner) labelHit(rec *record) bool {
	defer r.enter()()
	return r.shellVerbHit(rec, labelVerbs)
}

// packHit is owner_rule.pack_hit.
func (r *runner) packHit(rec *record) bool {
	defer r.enter()()
	return r.shellVerbHit(rec, packVerbs)
}
