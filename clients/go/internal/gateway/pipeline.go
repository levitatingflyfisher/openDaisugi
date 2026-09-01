package gateway

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	ExternalTier = "tier-switchyard"
	OffTier      = "tier-off"
	// UnknownTarget is the model an external turn is booked under when
	// the router did not name a clear target.
	UnknownTarget = "unknown"
	maxSessions   = 4096
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
	case ModeExternal:
		if g.External == nil {
			return nil, errors.New("router_mode 'external' needs an ExternalRouterConfig")
		}
		for k, v := range g.External.Prices {
			prices[k] = v
		}
	default:
		return nil, fmt.Errorf("router_mode must be one of rules, external, off, not %s", pystr.Repr(g.RouterMode))
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
}

// Prepare is Gateway.prepare. An error is a raise: the proxy then
// forwards the original bytes untouched.
func (g *Gateway) Prepare(body any) (*Prepared, error) {
	started := time.Now()
	p, err := g.prepare(body)
	if p != nil {
		p.Started = started
	}
	return p, err
}

func (g *Gateway) prepare(body any) (*Prepared, error) {
	switch g.RouterMode {
	case ModeExternal:
		return g.prepareExternal(body)
	case ModeOff:
		return g.prepareOff(body)
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
	sticky, has := g.sessions[key]
	if !has {
		sticky = nil
	}
	d, err := RouteTurn(body, g.CheapModel, g.LocalModel, sticky)
	if err != nil {
		return nil, err
	}
	if !has {
		if len(g.sessionKeys) >= maxSessions {
			delete(g.sessions, g.sessionKeys[0])
			g.sessionKeys = g.sessionKeys[1:]
		}
		g.sessionKeys = append(g.sessionKeys, key)
	}
	g.sessions[key] = d.Model
	out := copyObject(body.(*pyjson.Object))
	if d.Downgraded {
		out.Set("model", d.Model)
	}
	return &Prepared{Decision: d, Outbound: out, Task: task, Ask: ask}, nil
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
