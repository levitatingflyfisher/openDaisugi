package pathways

import (
	"errors"
	"strings"
	"unicode"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/pyyaml"
)

// ErrNotYet is a part of a command this binary does not carry.
var ErrNotYet = errors.New("not in this binary yet")

// exportSkill is portability._export_skill: YAML frontmatter as
// yaml.safe_dump writes it, then the skill body and its inputs.
func exportSkill(p *Pathway, version string) (string, error) {
	front := pyjson.NewObject().
		Set("name", slug(p.Task())).
		Set("description", p.Task()).
		Set("daisugi", pyjson.NewObject().
			Set("opendaisugi_version", version).
			Set("schema_version", pyjson.Int{Text: "1"}).
			Set("pathway", JSONMode(p.Obj)))
	head, why := pyyaml.SafeDump(front)
	if why != nil {
		return "", errors.Join(ErrUnreadable, why)
	}
	return "---\n" + head + "---\n\n" + skillBody(p) + "\n\n" + inputsMD(p), nil
}

// slug is portability._slug: str.isalnum() characters kept, every other
// one a dash, runs of dashes made one, 60 code points at most.
func slug(text string) string {
	var b strings.Builder
	for _, r := range pystr.Runes(pystr.Lower(text)) {
		if isAlnum(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = pystr.Slice(s, 0, 60)
	if s == "" {
		return "pathway"
	}
	return s
}

// isAlnum is str.isalnum() for one code point: a letter or a number.
func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

func skillBody(p *Pathway) string {
	perms := obj(obj(p.Obj.Value("envelope")).Value("permissions"))
	lines := []string{
		"# " + p.Task(),
		"",
		"A Z3-verified pathway compiled from successful journal traces by openDaisugi.",
		"On import, the plan template is re-verified against the envelope declared",
		"in this skill's frontmatter; imports fail closed if verification fails.",
		"",
		"## What this pathway does",
		"",
		p.Task(),
		"",
		"## Verified permissions",
		"",
		"- shell: `" + pyBool(perms.Value("shell")) + "` (allowlist: `" + orEmptyList(perms.Value("shell_allowlist")) + "`)",
		"- file_read: `" + orEmptyList(perms.Value("file_read")) + "`",
		"- file_write: `" + orEmptyList(perms.Value("file_write")) + "`",
		"- network: `" + pyBool(perms.Value("network")) + "` (hosts: `" + orEmptyList(perms.Value("network_hosts")) + "`)",
		"",
		"## Usage",
		"",
		"Install with:",
		"",
		"```bash",
		"daisugi pathways import path/to/this-skill.md",
		"```",
		"",
		"openDaisugi will re-verify the plan template against the envelope above.",
		"If verification passes, the pathway is admitted to the local PathwayStore",
		"and becomes a Tier-0 cache hit for matching tasks.",
		"",
		"## Graduating this into a polished skill",
		"",
		"openDaisugi *distilled* this from real successes; it doesn't author",
		"triggering descriptions or evals. To turn it into a durable, well-triggering",
		"skill, hand this file to a skill-authoring tool (e.g. `skill-creator`): keep",
		"the verified plan + envelope, and let it add the name, description, and evals.",
	}
	return strings.Join(lines, "\n") + "\n"
}

// inputsMD is portability._render_inputs_md.
func inputsMD(p *Pathway) string {
	params := p.Obj.Value("parameters").([]any)
	if len(params) == 0 {
		return "## Inputs\n\n" +
			"None — this is a **fixed** pathway. When its steps are shell/file/" +
			"network it runs verbatim with zero inference; reuse just replays it."
	}
	lines := []string{
		"## Inputs",
		"",
		"A **typed skill**: reuse binds these holes for the specific task, then " +
			"re-verifies the concrete plan against your envelope. A bound value may " +
			"change the data, never the capability head.",
		"",
	}
	for _, x := range params {
		po := obj(x)
		obs := strList(po.Value("observed"))
		if len(obs) > 3 {
			obs = obs[:3]
		}
		var ex []string
		for _, v := range obs {
			ex = append(ex, "`"+v+"`")
		}
		examples := strings.Join(ex, ", ")
		if examples == "" {
			examples = "—"
		}
		lines = append(lines, "- **"+po.Value("name").(string)+"** — fills the `"+po.Value("field").(string)+
			"` of a `"+po.Value("head").(string)+"` step; e.g. "+examples)
	}
	return strings.Join(lines, "\n")
}

// ParseSkill is parse_bundle for skill markdown: the frontmatter between
// the first "---\n" and the next "\n---\n", read as yaml.safe_load reads
// it, and its daisugi key taken as the bundle. A frontmatter not in the
// form yaml.safe_dump writes is not read by this binary (ErrNotYet).
func ParseSkill(text, source string) (*Pathway, error) {
	text = lstrip(text)
	_, rest, ok := strings.Cut(text, "---\n")
	if !ok {
		return nil, &ImportError{"SCHEMA_INCOMPATIBLE", "unterminated YAML frontmatter"}
	}
	front, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return nil, &ImportError{"SCHEMA_INCOMPATIBLE", "unterminated YAML frontmatter"}
	}
	data, why := pyyaml.LoadDumped(front)
	if why != nil {
		return nil, errors.Join(ErrNotYet, why)
	}
	o, isObj := data.(*pyjson.Object)
	if !isObj {
		return nil, errors.Join(ErrNotYet, errors.New("the frontmatter is not a mapping"))
	}
	bundle, present := o.Get("daisugi")
	if !present {
		return nil, &ImportError{"SCHEMA_INCOMPATIBLE", "skill frontmatter is missing the 'daisugi' key"}
	}
	return pathwayFromBundle(bundle, source)
}
