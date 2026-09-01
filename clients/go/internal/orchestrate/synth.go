package orchestrate

import (
	"sort"
	"strings"

	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/supervise"
)

// DefaultSynthModel is synthesizer._DEFAULT_MODEL.
const DefaultSynthModel = gateway.DefaultCheapModel

// synthSystem is synthesizer.SYNTHESIZER_SYSTEM_PROMPT.
const synthSystem = `You are a synthesizer. You are given the user's original request and the outputs
of the steps that were run to fulfill it. Write the single, complete final answer
the user asked for, drawing only on the step outputs. Do not mention the steps,
the plan, or that you are synthesizing — just give the answer.
`

// Answer is synthesizer._Answer.
var Answer = &pmodel.Model{Name: "_Answer", Fields: []pmodel.Field{
	{Name: "answer", Schema: pmodel.Str{}, Required: true},
}}

type stepOutput struct{ id, kind, status, output string }

// collectOutputs is synthesizer.collect_outputs: each outcome with its
// step's kind, in plan order.
func collectOutputs(sess *supervise.Session, steps []*pyjson.Object) []stepOutput {
	kind := map[string]string{}
	order := map[string]int{}
	for i, s := range steps {
		id := str(s, "id")
		kind[id] = str(s, "type")
		order[id] = i
	}
	outs := make([]stepOutput, len(sess.Steps))
	for i, o := range sess.Steps {
		k, ok := kind[o.StepID]
		if !ok {
			k = "?"
		}
		outs[i] = stepOutput{o.StepID, k, o.Status, o.Stdout}
	}
	pos := func(id string) int {
		if n, ok := order[id]; ok {
			return n
		}
		return len(order)
	}
	sort.SliceStable(outs, func(i, j int) bool { return pos(outs[i].id) < pos(outs[j].id) })
	return outs
}

// deterministic is synthesizer._deterministic_answer.
func deterministic(prompt string, outs []stepOutput) string {
	var ok []stepOutput
	for _, o := range outs {
		if o.status == supervise.Succeeded && pystr.Strip(o.output) != "" {
			ok = append(ok, o)
		}
	}
	if len(ok) == 0 {
		return "No step produced output for: " + prompt
	}
	lines := []string{"Results for: " + prompt, ""}
	for _, o := range ok {
		lines = append(lines, "["+o.id+" · "+o.kind+"]", pystr.Strip(o.output), "")
	}
	return pystr.RStrip(strings.Join(lines, "\n"))
}

// Synthesize is synthesizer.synthesize: the answer, and whether a model
// wrote it. It never fails: any failure gives the deterministic answer.
func Synthesize(c *llm.Client, prompt string, sess *supervise.Session, steps []*pyjson.Object, useLLM bool) (string, bool) {
	outs := collectOutputs(sess, steps)
	if !useLLM || c.Preflight(DefaultSynthModel) != nil {
		return deterministic(prompt, outs), false
	}
	blocks := make([]string, len(outs))
	for i, o := range outs {
		blocks[i] = "### step " + o.id + " (" + o.kind + ", " + o.status + ")\n" + pystr.Strip(o.output)
	}
	user := "Original request:\n" + prompt + "\n\nStep outputs:\n" + strings.Join(blocks, "\n\n")
	reply, err := c.Structured(llm.Call{Model: DefaultSynthModel, System: synthSystem, User: user,
		Response: llm.Schema{Name: "_Answer", Model: Answer}, DefaultRetries: true})
	if err != nil {
		return deterministic(prompt, outs), false
	}
	a, _ := reply.Value("answer").(string)
	a = pystr.Strip(a)
	if a == "" {
		return deterministic(prompt, outs), false
	}
	return a, true
}
