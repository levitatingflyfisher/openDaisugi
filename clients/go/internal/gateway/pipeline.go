package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is gateway_pipeline.Gateway: prepare before the upstream
// call, finish after it.

// Router modes and the tiers they journal under.
const (
	ModeRules    = "rules"
	ModeExternal = "external"
	ModeOff      = "off"
	ModeKnn      = "knn"
	ModeModel    = "model"
	ExternalTier = "tier-switchyard"
	OffTier      = "tier-off"
	// UnknownTarget is the model an external turn is booked under when
	// the router did not name a clear target.
	UnknownTarget = "unknown"
	maxSessions   = 4096
	// NotAsked is the reason the router model was not asked.
	NotAsked = "not asked"
)

// sessMu guards the sticky tables; fileMu orders the journal and answer-file
// writes. Neither is held across the embedder or the outcome table, so a slow
// embedding or a busy database never blocks the other turns.
var (
	sessMu sync.Mutex
	fileMu sync.Mutex
)

// External is ExternalRouterConfig.
type External struct {
	RouteID         string
	CapableTarget   string
	EfficientTarget string
	Prices          Prices
}

// Gateway is gateway_pipeline.Gateway.
type Gateway struct {
	CheapModel     string
	LocalModel     string
	Prices         Prices
	RouterMode     string
	External       *External
	Knn            *Knn   // the knn router, for ModeKnn and ModeModel
	RouterModel    string // the model that picks, for ModeModel
	GraderModel    string // the model that grades finished turns, or empty
	JournalPath    string // "" when journalling is off
	AnswersPath    string // "" unless answers are captured
	CaptureAnswers bool
	MaxAnswers     int

	// The sticky table: conversation key to the last routed model, first
	// in first out past maxSessions.
	sessionKeys []string
	sessions    map[string]any
}

// NewGateway is Gateway(...) with __post_init__: a local model prices at
// zero, and external mode adds its targets' prices.
func NewGateway(g Gateway) (*Gateway, error) {
	if g.CheapModel == "" {
		g.CheapModel = DefaultCheapModel
	}
	if g.Prices == nil {
		g.Prices = DefaultPrices()
	}
	if g.RouterMode == "" {
		g.RouterMode = ModeRules
	}
	if g.MaxAnswers == 0 {
		g.MaxAnswers = 1000
	}
	prices := Prices{}
	for k, v := range g.Prices {
		prices[k] = v
	}
	if g.LocalModel != "" {
		if _, has := prices[g.LocalModel]; !has {
			prices[g.LocalModel] = Price{}
		}
	}
	switch g.RouterMode {
	case ModeRules, ModeOff:
	case ModeKnn:
		if g.Knn == nil {
			return nil, errors.New("router_mode 'knn' needs a KnnRouter")
		}
	case ModeModel:
		if g.Knn == nil || g.RouterModel == "" {
			return nil, errors.New("router_mode 'model' needs a KnnRouter and a router_model")
		}
	case ModeExternal:
		if g.External == nil {
			return nil, errors.New("router_mode 'external' needs an ExternalRouterConfig")
		}
		for k, v := range g.External.Prices {
			prices[k] = v
		}
	default:
		return nil, fmt.Errorf("router_mode must be one of rules, external, off, knn, model, not %s", pystr.Repr(g.RouterMode))
	}
	g.Prices = prices
	g.sessions = map[string]any{}
	return &g, nil
}

// Prepared is PreparedTurn.
type Prepared struct {
	Decision Decision
	Outbound *pyjson.Object
	Task     string
	Ask      string
	// Started is when Prepare began; Finish books the time from here as
	// the record's elapsed_ms. The zero time books none.
	Started time.Time
	// Pane and Floor are the coppice pane and floor the turn came from,
	// or empty. The proxy takes them from the request path; Finish stamps
	// the record.
	Pane, Floor string
	// knn mode only: the task vector, the embedder that made it and the
	// conversation key. Finish books them; a turn with no vector is not booked.
	KnnVec       []float32
	KnnEmbedder  string
	Conversation string
	// Failed marks an upstream error or a refusal. The proxy sets it before
	// Finish; the knn router books such a turn as bad.
	Failed bool
}

// Prepare is Gateway.prepare. An error is a raise: the proxy then
// forwards the original bytes untouched.
func (g *Gateway) Prepare(body any) (*Prepared, error) { return g.PrepareWith(body, nil) }

// PrepareWith is Gateway.prepare(body, choice): in model mode, choice is
// the router model's answer.
func (g *Gateway) PrepareWith(body any, choice *Choice) (*Prepared, error) {
	started := time.Now()
	p, err := g.prepare(body, choice)
	if p != nil {
		p.Started = started
	}
	return p, err
}

func (g *Gateway) prepare(body any, choice *Choice) (*Prepared, error) {
	switch g.RouterMode {
	case ModeExternal:
		return g.prepareExternal(body)
	case ModeOff:
		return g.prepareOff(body)
	case ModeKnn:
		return g.prepareKnn(body, nil)
	case ModeModel:
		if choice == nil {
			choice = &Choice{Why: NotAsked}
		}
		return g.prepareKnn(body, choice)
	}
	task, err := latestUserText(body)
	if err != nil {
		return nil, err
	}
	ask, err := newUserText(body)
	if err != nil {
		return nil, err
	}
	key, err := conversationKey(body)
	if err != nil {
		return nil, err
	}
	sticky, has := g.stickyOf(key)
	d, err := RouteTurn(body, g.CheapModel, g.LocalModel, sticky)
	if err != nil {
		return nil, err
	}
	g.remember(key, has, d.Model)
	out := copyObject(body.(*pyjson.Object))
	if d.Downgraded {
		out.Set("model", d.Model)
	}
	return &Prepared{Decision: d, Outbound: out, Task: task, Ask: ask}, nil
}

func (g *Gateway) stickyOf(key string) (any, bool) {
	sessMu.Lock()
	defer sessMu.Unlock()
	sticky, has := g.sessions[key]
	if !has {
		sticky = nil
	}
	return sticky, has
}

func (g *Gateway) remember(key string, has bool, model any) {
	sessMu.Lock()
	defer sessMu.Unlock()
	if _, now := g.sessions[key]; now {
		has = true
	}
	if !has {
		if len(g.sessionKeys) >= maxSessions {
			delete(g.sessions, g.sessionKeys[0])
			g.sessionKeys = g.sessionKeys[1:]
		}
		g.sessionKeys = append(g.sessionKeys, key)
	}
	g.sessions[key] = model
}

// prepareKnn is Gateway._prepare_knn: route by the outcomes of similar
// past turns. With too few known neighbours, or no vector for the task,
// the rules router decides and the reason says why.
func (g *Gateway) prepareKnn(body any, choice *Choice) (*Prepared, error) {
	task, err := latestUserText(body)
	if err != nil {
		return nil, err
	}
	ask, err := newUserText(body)
	if err != nil {
		return nil, err
	}
	key, err := conversationKey(body)
	if err != nil {
		return nil, err
	}
	sticky, has := g.stickyOf(key)
	requested, err := get(body, "model", "")
	if err != nil {
		return nil, err
	}
	rules := func() (Decision, error) { return RouteTurn(body, g.CheapModel, g.LocalModel, sticky) }
	fallback := func(prefix string) (Decision, error) {
		d, err := rules()
		if err != nil {
			return d, err
		}
		d.Reason = prefix + "; rules router: " + d.Reason
		return d, nil
	}
	var vec []float32
	identity := ""
	var d Decision
	reqStr, isString := requested.(string)
	switch {
	case pystr.Strip(task) == "":
		d, err = rules()
	case !isString || reqStr == "":
		d, err = fallback("knn: the request names no model")
	default:
		var eerr error
		// In model mode the vector only books the turn, and a continuation is not booked.
		if choice == nil || pystr.Strip(ask) != "" {
			vec, identity, eerr = g.Knn.Embed(task)
			if eerr != nil {
				vec, identity = nil, ""
			}
		}
		if choice != nil {
			d, err = g.modelDecision(body, choice, sticky, task, ask, reqStr)
			break
		}
		if eerr != nil {
			d, err = fallback("knn: could not embed the task")
			break
		}
		rows, rerr := g.Knn.Store.Recent(identity, KnnMaxRows)
		if rerr != nil {
			rows = nil
		}
		kd := KnnDecide(vec, rows, reqStr, KnnCandidates(reqStr, g.CheapModel, g.LocalModel, g.Prices),
			g.Prices, g.Knn.Threshold, KnnK, KnnMinKnown)
		if kd.Fallback {
			d, err = fallback(kd.Reason)
		} else {
			d, err = g.knnDecision(kd, body, sticky, task, reqStr)
		}
	}
	if err != nil {
		return nil, err
	}
	g.remember(key, has, d.Model)
	out := copyObject(body.(*pyjson.Object))
	if d.Downgraded {
		out.Set("model", d.Model)
	}
	return &Prepared{Decision: d, Outbound: out, Task: task, Ask: ask, KnnVec: vec, KnnEmbedder: identity,
		Conversation: key}, nil
}

// modelDecision is Gateway._prepare_model's decision: the router model
// picked choice.Name, or failed with choice.Why. A name outside the list
// goes to the rules router. A continuation stays on the conversation's
// last model.
func (g *Gateway) modelDecision(body any, choice *Choice, sticky any, task, ask, requested string) (Decision, error) {
	cands := KnnCandidates(requested, g.CheapModel, g.LocalModel, g.Prices)
	picked, why := choice.Name, choice.Why
	how := "model: " + picked + " picked by " + g.RouterModel + " from " + strings.Join(cands, ", ")
	if last, isStr := sticky.(string); picked == "" && why == NotAsked && pystr.Strip(ask) == "" &&
		isStr && slices.Contains(cands, last) {
		picked, how = last, "model: "+last+" kept from the last turn of this conversation"
	} else if picked != "" && !slices.Contains(cands, picked) {
		picked, why = "", "the router model named a model that is not allowed"
	}
	if picked == "" {
		d, err := RouteTurn(body, g.CheapModel, g.LocalModel, sticky)
		if err != nil {
			return d, err
		}
		d.Reason = "model: " + why + "; rules router: " + d.Reason
		return d, nil
	}
	return g.knnDecision(KnnDecision{Model: picked, Reason: how}, body, sticky, task, requested)
}

// knnDecision is Gateway._knn_decision: a Decision from a knn choice, with
// ADR-0015 stickiness on top.
func (g *Gateway) knnDecision(kd KnnDecision, body any, sticky any, task, requested string) (Decision, error) {
	difficulty := EstimateDifficulty(task)
	model := kd.Model
	if model == requested {
		return Decision{Tier: "tier2-frontier", Model: requested, RequestedModel: requested,
			Difficulty: difficulty, Reason: kd.Reason}, nil
	}
	if model != g.LocalModel && !eqStr(sticky, model) {
		prefix, err := EstimatePrefixTokens(body)
		if err != nil {
			return Decision{}, err
		}
		if prefix >= StickyPrefixThreshold {
			return Decision{Tier: "tier2-frontier", Model: requested, RequestedModel: requested,
				Difficulty: difficulty,
				Reason: fmt.Sprintf("%s; sticky: ~%d prefix tokens cached against the requested model, so it stays",
					kd.Reason, prefix)}, nil
		}
	}
	tier := "tier1-cheap"
	if g.LocalModel != "" && model == g.LocalModel {
		tier = "tier1-local"
	}
	return Decision{Tier: tier, Model: model, RequestedModel: requested, Difficulty: difficulty,
		Downgraded: true, Reason: kd.Reason}, nil
}

func copyObject(o *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObjectCap(o.Len() + 1)
	for _, k := range o.Keys() {
		out.Set(k, o.Value(k))
	}
	return out
}

func (g *Gateway) prepareExternal(body any) (*Prepared, error) {
	requested, err := get(body, "model", "")
	if err != nil {
		return nil, err
	}
	if !isStr(requested) {
		requested = ""
	}
	route := g.External.RouteID
	d := Decision{Tier: ExternalTier, Model: route, RequestedModel: requested,
		Reason: fmt.Sprintf("the external router picks the model; sent as route %s", pystr.Repr(route))}
	out := copyObject(body.(*pyjson.Object))
	out.Set("model", route)
	task, err := latestUserText(body)
	if err != nil {
		return nil, err
	}
	ask, err := newUserText(body)
	if err != nil {
		return nil, err
	}
	return &Prepared{Decision: d, Outbound: out, Task: task, Ask: ask}, nil
}

func (g *Gateway) prepareOff(body any) (*Prepared, error) {
	requested, err := get(body, "model", "")
	if err != nil {
		return nil, err
	}
	if !isStr(requested) {
		requested = ""
	}
	d := Decision{Tier: OffTier, Model: requested, RequestedModel: requested,
		Reason: "routing is off; the turn goes unchanged and is only metered"}
	out := copyObject(body.(*pyjson.Object))
	task, err := latestUserText(body)
	if err != nil {
		return nil, err
	}
	ask, err := newUserText(body)
	if err != nil {
		return nil, err
	}
	return &Prepared{Decision: d, Outbound: out, Task: task, Ask: ask}, nil
}

// ExternalDecision is _external_decision: the decision an external turn
// is booked with once the response names its target (nil when it named
// none clearly).
func (g *Gateway) ExternalDecision(p *Prepared, served *string) (Decision, error) {
	d := p.Decision
	ext := g.External
	if served == nil || (ext != nil && *served == ext.RouteID) {
		d.Model = UnknownTarget
		d.Downgraded = false
		d.Reason = "the external router did not name a clear target; booked no saving"
		return d, nil
	}
	down := false
	if ext != nil && *served == ext.EfficientTarget {
		ok, err := g.Prices.StrictlyCheaper(*served, d.RequestedModel)
		if err != nil {
			return Decision{}, err
		}
		down = ok
	}
	d.Model = *served
	d.Downgraded = down
	if down {
		d.Reason = "the external router served the efficient target " + *served
	} else {
		d.Reason = "the external router served " + *served + "; booked no saving"
	}
	return d, nil
}

// Finish is Gateway.finish: measure the turn, append its record, and
// capture the answer when capture is on. An error is a raise the proxy
// swallows: the turn was served, the line is not written.
func (g *Gateway) Finish(p *Prepared, usage any, answer string, now time.Time) (Record, error) {
	d := p.Decision
	if d.Downgraded {
		ok, err := g.Prices.StrictlyCheaper(d.Model, d.RequestedModel)
		if err != nil {
			return Record{}, err
		}
		if !ok {
			d.Downgraded = false
			d.Reason += "; the price table shows no saving, so none is booked"
		}
	}
	s, err := g.Prices.measure(d, usage)
	if err != nil {
		return Record{}, err
	}
	rec, err := recordTurn(d, s, p.Task, p.Ask, now)
	if err == nil && !p.Started.IsZero() {
		ms := pyjson.Round(float64(time.Since(p.Started).Nanoseconds())/1e6, 3)
		rec.Elapsed = &ms
	}
	if err != nil {
		return Record{}, err
	}
	rec.Pane, rec.Floor = p.Pane, p.Floor
	if g.RouterMode == ModeKnn || g.RouterMode == ModeModel {
		rec.Reason = d.Reason
		rec.TurnID = g.bookKnn(p, d, s, rec.Elapsed, now)
	}
	fileMu.Lock()
	defer fileMu.Unlock()
	if g.JournalPath != "" {
		if err := appendLine(g.JournalPath, rec.JSON()); err != nil {
			return Record{}, err
		}
	}
	if g.CaptureAnswers && g.AnswersPath != "" && answer != "" && pystr.Strip(p.Ask) != "" {
		// Best effort, as the oracle's: a failure never breaks the turn.
		_ = CaptureAnswer(g.AnswersPath, g.MaxAnswers, p.Task, answer, now)
	}
	return rec, nil
}

// bookKnn is Gateway._book_knn: book one knn turn in the outcome table.
// It is best effort and never breaks the turn.
func (g *Gateway) bookKnn(p *Prepared, d Decision, s Saving, elapsed *float64, now time.Time) string {
	if g.Knn == nil || p.KnnVec == nil || pystr.Strip(p.Ask) == "" {
		return ""
	}
	model, ok := d.Model.(string)
	if !ok {
		return ""
	}
	id, err := g.Knn.Record(p.Conversation, p.Task, p.KnnVec, p.KnnEmbedder, model, s.Actual.Dollars,
		elapsed, p.Failed, float64(now.UnixNano())/1e9)
	if err != nil {
		return ""
	}
	return id
}

// appendLine is `open(path, "a").write(line + "\n")` after mkdir -p.
func appendLine(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(line + "\n")
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
