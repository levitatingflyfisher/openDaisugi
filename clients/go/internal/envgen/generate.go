package envgen

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"daisugi-verify/internal/embed/potion"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/verify"
)

// ErrUnported is a path of the oracle this binary does not take: the
// caller says "... is not in this binary yet." and changes nothing more.
var ErrUnported = errors.New("is not in this binary yet")

// DefaultModel is generate_envelope's default model.
const DefaultModel = "anthropic/claude-sonnet-4-20250514"

// SelfConsistencyTimeoutMs is check_envelope_self_consistency's default
// budget, the one generate_envelope uses.
const SelfConsistencyTimeoutMs = 500

// Refiner is the journal's get_refinements_by_key.
type Refiner interface {
	RefinementsByKey(key string) ([]*pyjson.Object, error)
}

// Options are generate_envelope's arguments.
type Options struct {
	Task      string
	Context   *string
	Parent    *pyjson.Object
	Summarize bool
	Cache     *Cache
	// Store, with MatcherKey and Potion, is the compiled-pathway store
	// (Tier-0). Threshold nil is find's default.
	Store      *pathways.Store
	MatcherKey string
	Potion     potion.Env
	Threshold  *float64
	Journal    Refiner
	Stakes     string
	LowStakes  *pyjson.Object
	// Models is the ladder; Single is a bare-string model.
	Models       []string
	Single       bool
	Thinking     string
	Tier1        *Tier1
	MaxRetries   int
	MaxTaskChars int
	LLM          *llm.Client
}

// Defaults fills what generate_envelope defaults.
func (o *Options) Defaults() {
	if o.Stakes == "" {
		o.Stakes = "medium"
	}
	if o.Models == nil {
		o.Models, o.Single = []string{DefaultModel}, true
	}
	if o.Thinking == "" {
		o.Thinking = "standard"
	}
}

// Result is the envelope, and the stale-embeddings warning Tier-0's find
// gave, if any.
type Result struct {
	Envelope    *pyjson.Object
	FindWarning string
}

// errClass is the class of a failed model call: the HTTP client raises
// ModelCallError, the claude-code client EnvelopeGenerationError.
func errClass(c *llm.Client) string {
	if c.Backend() == "claude-code" {
		return "EnvelopeGenerationError"
	}
	return "ModelCallError"
}

var openaiReasoning = regexp.MustCompile(`^(openai/)?(o[1-9]|o4)`)

// thinkingOpts is thinking.thinking_kwargs for the two keys the model
// clients send (Gemini's thinking_config reaches no wire).
func thinkingOpts(model, budget string) (thinking int, effort string) {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "anthropic/") || strings.HasPrefix(m, "claude-"):
		if budget == "deep" {
			return 16000, ""
		}
	case openaiReasoning.MatchString(m):
		return 0, map[string]string{"light": "low", "standard": "medium", "deep": "high"}[budget]
	}
	return 0, ""
}

// checkSelfConsistency is the check selfConsistent runs; a test swaps it
// to make it answer unknown.
var checkSelfConsistency = verify.CheckEnvelopeSelfConsistency

// selfConsistent is check_envelope_self_consistency, failing closed: a
// check that did not finish, or an envelope it cannot read, is not
// consistent, as in the oracle (K1-2). The text is the first violation's
// message.
func selfConsistent(env *pyjson.Object) (bool, string) {
	e, err := verify.ParseEnvelope(json.RawMessage(pathways.DumpJSON(env)))
	if err != nil {
		return false, "the envelope does not read: " + err.Error()
	}
	vs, timeout := checkSelfConsistency(e, SelfConsistencyTimeoutMs)
	if timeout != "" {
		return false, timeout
	}
	if len(vs) > 0 {
		return false, vs[0].Message
	}
	return true, ""
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case *pyjson.Object:
		o := pyjson.NewObject()
		for _, k := range x.Keys() {
			o.Set(k, deepCopy(x.Value(k)))
		}
		return o
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

// DeepCopy is model_copy(deep=True) of a model_dump() value.
func DeepCopy(o *pyjson.Object) *pyjson.Object { return deepCopy(o).(*pyjson.Object) }

func timestamp(rec *pyjson.Object) float64 {
	f, _ := floatOf(rec.Value("timestamp"))
	return f
}

// hintsBlock is _refinement_hints_block.
func hintsBlock(records []*pyjson.Object) string {
	if len(records) == 0 {
		return ""
	}
	var order []string
	newest := map[string]float64{}
	stage := map[string]string{}
	for _, rec := range records {
		ts := timestamp(rec)
		for _, v := range listOf(rec.Value("violations")) {
			vo := obj(v)
			msg, _ := vo.Value("message").(string)
			if old, seen := newest[msg]; !seen {
				order = append(order, msg)
				newest[msg] = ts
			} else if ts > old {
				newest[msg] = ts
			}
			if _, seen := stage[msg]; !seen {
				stage[msg], _ = vo.Value("stage").(string)
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return -newest[order[i]] < -newest[order[j]] })
	if len(order) > 10 {
		order = order[:10]
	}
	lines := []string{"", "## Prior Rejections", "",
		"Previous plans verified against envelopes for this task were rejected.",
		"Generate an envelope that prevents these violations:", ""}
	for _, m := range order {
		lines = append(lines, "- ["+stage[m]+"] "+m)
	}
	return strings.Join(lines, "\n")
}

func (o *Options) refinements(key string) []*pyjson.Object {
	if o.Journal == nil {
		return nil
	}
	recs, err := o.Journal.RefinementsByKey(key)
	if err != nil {
		return nil
	}
	return recs
}

func (o *Options) key(model string, tier1 *string) string {
	var parent *string
	if o.Parent != nil {
		if id, ok := o.Parent.Value("id").(string); ok {
			parent = &id
		}
	}
	return CacheKey(KeyArgs{Task: o.Task, Context: o.Context, Model: model, Parent: parent,
		Summarize: o.Summarize, Thinking: o.Thinking, Tier1: tier1})
}

// adopt stamps parent_envelope and checks inheritance, as the oracle does
// for a Tier-1 or Tier-2 envelope.
func (o *Options) adopt(env *pyjson.Object) error {
	if o.Parent == nil {
		return nil
	}
	env.Set("parent_envelope", o.Parent.Value("id"))
	if vs := VerifyInheritance(env, o.Parent); len(vs) > 0 {
		return &InheritanceError{Messages: vs}
	}
	return nil
}

// Generate is generate_envelope. Its errors are *PyError (the class the
// oracle raises and its text), *InheritanceError, *ErrCacheRow, or wrap
// ErrUnported.
func Generate(o Options) (Result, error) {
	o.Defaults()
	var res Result
	if o.Task == "" || pystr.Strip(o.Task) == "" {
		return res, &PyError{"ValueError", "Task must be a non-empty string."}
	}
	n := utf8.RuneCountInString(o.Task)
	if o.Context != nil {
		n += utf8.RuneCountInString(*o.Context)
	}
	if n > o.MaxTaskChars {
		return res, &PyError{"TaskTooLongError", fmt.Sprintf("Task + context is %d chars (limit: %d). "+
			"Summarize before passing, or increase max_task_chars.", n, o.MaxTaskChars)}
	}
	if o.Stakes == "low" {
		if o.LowStakes == nil {
			return res, &PyError{"LowStakesNotConfigured", "stakes='low' requires a configured envelope. Pass " +
				"low_stakes_envelope=... or construct the facade via Daisugi.with_default_low_stakes()."}
		}
		res.Envelope = DeepCopy(o.LowStakes)
		return res, nil
	}
	if len(o.Models) == 0 {
		return res, &PyError{"ValueError", "model must be a non-empty string or non-empty list of strings."}
	}

	user := "Task: " + o.Task
	if o.Context != nil && *o.Context != "" {
		user += "\n\nContext:\n" + *o.Context
	}
	if o.Summarize {
		user += summarizeInstruction
	}

	// Tier-0: a compiled pathway.
	if o.Store != nil && o.Stakes != "high" {
		th := -1.0
		if o.Threshold != nil {
			th = *o.Threshold
		}
		r, err := o.Store.Find(o.Task, o.MatcherKey, o.Potion, th)
		if errors.Is(err, pathways.ErrNotCarried) {
			return res, fmt.Errorf("the %s matcher %w", o.MatcherKey, ErrUnported)
		}
		res.FindWarning = r.Warning
		if err == nil && r.Match != nil {
			p := r.Match.Pathway
			if err := o.Store.IncrementHit(p.ID()); err != nil {
				return res, err
			}
			env := DeepCopy(obj(p.Obj.Value("envelope")))
			env.Set("generated_by", "compiled-pathway:"+p.ID())
			res.Envelope = env
			return res, nil
		}
	}

	// Tier-1: the local-model slot.
	if o.Tier1 != nil && o.Stakes != "high" {
		first := o.Models[0]
		name := o.Tier1.Name
		t1key := o.key(first, &name)
		if o.Cache != nil {
			cached, err := o.Cache.Get(t1key)
			if err != nil {
				return res, err
			}
			if cached != nil {
				if cached.Value("cache_key") == nil {
					cached.Set("cache_key", t1key)
				}
				res.Envelope = cached
				return res, nil
			}
		}
		if env := o.Tier1.Generate(o.LLM, o.Task, o.Context); env != nil {
			if ok, _ := selfConsistent(env); ok {
				env.Set("generated_by", "tier1:"+name)
				if err := o.adopt(env); err != nil {
					return res, err
				}
				env.Set("cache_key", t1key)
				if o.Cache != nil {
					o.Cache.Put(env, t1key)
				}
				res.Envelope = env
				return res, nil
			}
		}
	}

	// The cache, every rung up front.
	if o.Cache != nil && o.Stakes != "high" {
		for _, rung := range o.Models {
			key := o.key(rung, nil)
			cached, err := o.Cache.Get(key)
			if err != nil {
				return res, err
			}
			if cached == nil {
				continue
			}
			at, has, err := o.Cache.InsertedAt(key)
			if err != nil {
				return res, err
			}
			recs := o.refinements(key)
			if has && len(recs) > 0 {
				newest := timestamp(recs[0])
				for _, r := range recs[1:] {
					newest = max(newest, timestamp(r))
				}
				if newest > at {
					o.Cache.Invalidate(key)
					break
				}
			}
			if cached.Value("cache_key") == nil {
				cached.Set("cache_key", key)
			}
			res.Envelope = cached
			return res, nil
		}
	}

	// The ladder.
	var lastClass, lastMsg string
	for _, rung := range o.Models {
		key := o.key(rung, nil)
		rungUser := user + hintsBlock(o.refinements(key))
		if e := o.LLM.Preflight(rung); e != nil {
			return res, &PyError{"LLMNotConfigured", e.Msg}
		}
		thinking, effort := thinkingOpts(rung, o.Thinking)
		env, err := o.LLM.Structured(llm.Call{Model: rung, System: SystemPrompt, User: rungUser,
			Response: llm.Schema{Name: "Envelope", Model: pmodel.Envelope}, MaxRetries: o.MaxRetries,
			ThinkingBudget: thinking, ReasoningEffort: effort})
		if err != nil {
			lastClass, lastMsg = errClass(o.LLM), err.Error()
			continue
		}
		if ok, why := selfConsistent(env); !ok {
			lastClass = "EnvelopeGenerationError"
			lastMsg = fmt.Sprintf("Model %s produced self-inconsistent envelope: %s", pystr.Repr(rung), why)
			continue
		}
		if err := o.adopt(env); err != nil {
			return res, err
		}
		env.Set("cache_key", key)
		if o.Cache != nil {
			o.Cache.Put(env, key)
		}
		res.Envelope = env
		return res, nil
	}
	if o.Single {
		return res, &PyError{lastClass, lastMsg}
	}
	return res, &PyError{"ModelLadderExhausted", fmt.Sprintf("All models in ladder exhausted: %s. Last error (%s): %s",
		pystr.ReprList(o.Models), lastClass, lastMsg)}
}
