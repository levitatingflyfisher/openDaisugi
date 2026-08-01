// Package verify is the Go port of the openDaisugi verifier core
// (src/opendaisugi/{models,shell_decompose,verify,interpreter_parse,dag,
// z3_checks,predicate,predicate_z3,vacuity,contracts,subsumption}.py).
//
// It is an INDEPENDENT reimplementation from the spec + oracle source, not a
// transliteration of the Python AST — see clients/PORTING-NOTES.md for the
// traps and clients/ADJUDICATIONS.md for every place this port knowingly
// disagrees with tree-sitter-bash's grammar (mvdan.cc/sh/v3 is used instead)
// while still MATCHING the oracle's verdicts.
package verify

import (
	"encoding/json"
	"fmt"
)

// --- Permission -------------------------------------------------------------

// Permission mirrors opendaisugi.models.Permission. Field defaults matter:
// Go's zero values (false, 0, nil) are NOT always the pydantic defaults, so
// every JSON-decoding entry point must start from DefaultPermission() and
// unmarshal on top of it (json.Unmarshal only overwrites keys present in the
// input, so absent keys keep the pre-filled default).
type Permission struct {
	FileRead                []string             `json:"file_read"`
	FileWrite               []string             `json:"file_write"`
	Network                 bool                 `json:"network"`
	NetworkHosts            []string             `json:"network_hosts"`
	Shell                   bool                 `json:"shell"`
	ShellAllowlist          []string             `json:"shell_allowlist"`
	ShellAllowDecomposition bool                 `json:"shell_allow_decomposition"`
	McpAllowlist            []string             `json:"mcp_allowlist"`
	CustomStepAllowlist     []string             `json:"custom_step_allowlist"`
	MaxExecutionTimeS       int                  `json:"max_execution_time_s"`
	MaxOutputSizeMB         int                  `json:"max_output_size_mb"`
	WorkspaceBounds         *[2][3]float64       `json:"workspace_bounds"`
	Obstacles               [][2][3]float64      `json:"obstacles"`
	VelocityLimit           *float64             `json:"velocity_limit"`
	JointLimits             map[string][2]float64 `json:"joint_limits"`
	TorqueLimit             *float64             `json:"torque_limit"`
}

func DefaultPermission() Permission {
	return Permission{
		FileRead:                []string{},
		FileWrite:               []string{},
		NetworkHosts:            []string{},
		ShellAllowlist:          []string{},
		McpAllowlist:            []string{},
		CustomStepAllowlist:     []string{},
		MaxExecutionTimeS:       30,
		MaxOutputSizeMB:         10,
		Obstacles:               [][2][3]float64{},
		JointLimits:             map[string][2]float64{},
	}
}

// UnmarshalJSON fills in pydantic defaults first, then overlays the JSON.
func (p *Permission) UnmarshalJSON(data []byte) error {
	*p = DefaultPermission()
	type alias Permission
	a := (*alias)(p)
	return json.Unmarshal(data, a)
}

// --- Invariant / Postcondition ----------------------------------------------

type Invariant struct {
	Type        string          `json:"type"`
	Target      *string         `json:"target"`
	Scope       *string         `json:"scope"`
	Description string          `json:"description"`
	Expr        json.RawMessage `json:"expr"`
	Enforce     bool            `json:"enforce"`
}

func (i *Invariant) UnmarshalJSON(data []byte) error {
	i.Enforce = true
	type alias Invariant
	a := (*alias)(i)
	return json.Unmarshal(data, a)
}

type Postcondition struct {
	Type        string          `json:"type"`
	Path        *string         `json:"path"`
	Expected    *int            `json:"expected"`
	Min         *float64        `json:"min"`
	Max         *float64        `json:"max"`
	Description *string         `json:"description"`
	Expr        json.RawMessage `json:"expr"`
	Enforce     bool            `json:"enforce"`
}

func (p *Postcondition) UnmarshalJSON(data []byte) error {
	p.Enforce = true
	type alias Postcondition
	a := (*alias)(p)
	return json.Unmarshal(data, a)
}

// --- FallbackStrategy ---------------------------------------------------------

type FallbackStrategy struct {
	Strategy          string `json:"strategy"`
	Model             string `json:"model"`
	IncludeRefinement bool   `json:"include_refinement"`
}

func DefaultFallbackStrategy() FallbackStrategy {
	return FallbackStrategy{
		Strategy:          "tier2_recompute",
		Model:             "anthropic/claude-sonnet-4-20250514",
		IncludeRefinement: true,
	}
}

func (f *FallbackStrategy) UnmarshalJSON(data []byte) error {
	*f = DefaultFallbackStrategy()
	type alias FallbackStrategy
	a := (*alias)(f)
	return json.Unmarshal(data, a)
}

// --- Envelope -----------------------------------------------------------------

type Envelope struct {
	ID                      string          `json:"id"`
	GeneratedBy             string          `json:"generated_by"`
	Task                    string          `json:"task"`
	Permissions             Permission      `json:"permissions"`
	Invariants              []Invariant     `json:"invariants"`
	Postconditions          []Postcondition `json:"postconditions"`
	Fallback                FallbackStrategy `json:"fallback"`
	ParentEnvelope          *string         `json:"parent_envelope"`
	TighteningOnly          bool            `json:"tightening_only"`
	Summary                 *string         `json:"summary"`
	CacheKey                *string         `json:"cache_key"`
	Stakes                  string          `json:"stakes"`
	ShellInterpreterPolicy  string          `json:"shell_interpreter_policy"`
}

func DefaultEnvelope() Envelope {
	return Envelope{
		Permissions:            DefaultPermission(),
		Invariants:             []Invariant{},
		Postconditions:         []Postcondition{},
		Fallback:               DefaultFallbackStrategy(),
		TighteningOnly:         true,
		Stakes:                 "low",
		ShellInterpreterPolicy: "surface",
	}
}

func (e *Envelope) UnmarshalJSON(data []byte) error {
	*e = DefaultEnvelope()
	type alias Envelope
	a := (*alias)(e)
	return json.Unmarshal(data, a)
}

// StrictStakes mirrors verify._STRICT_STAKES.
func (e Envelope) IsStrictStakes() bool {
	return e.Stakes == "high" || e.Stakes == "physical"
}

// ResolveStrict mirrors verify.resolve_strict.
func ResolveStrict(strict *bool, env Envelope) bool {
	if strict != nil {
		return *strict
	}
	return env.IsStrictStakes()
}

// --- Step (ActionStep) ---------------------------------------------------------

// Step is a duck-typed decode of any ActionStep subclass: Raw carries the
// FULL decoded JSON object (numbers as float64, matching what
// pydantic's .model_dump() would hand to Python's predicate evaluator), and
// ID/Type/DependsOn are pulled out for the hot paths (dag, permissions).
type Step struct {
	ID        string
	Type      string
	DependsOn []string
	Raw       map[string]interface{}
}

func (s *Step) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Raw = raw
	if t, ok := raw["type"].(string); ok {
		s.Type = t
	}
	if id, ok := raw["id"].(string); ok {
		s.ID = id
	}
	s.DependsOn = nil
	if deps, ok := raw["depends_on"].([]interface{}); ok {
		for _, d := range deps {
			if ds, ok := d.(string); ok {
				s.DependsOn = append(s.DependsOn, ds)
			}
		}
	}
	return nil
}

func (s Step) str(key string) (string, bool) {
	v, ok := s.Raw[key].(string)
	return v, ok
}

func (s Step) strDefault(key, def string) string {
	if v, ok := s.str(key); ok {
		return v
	}
	return def
}

func (s Step) boolean(key string) bool {
	v, _ := s.Raw[key].(bool)
	return v
}

func (s Step) stringSlice(key string) []string {
	arr, ok := s.Raw[key].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		if sv, ok := v.(string); ok {
			out = append(out, sv)
		}
	}
	return out
}

func (s Step) float(key string) (float64, bool) {
	v, ok := s.Raw[key].(float64)
	return v, ok
}

func (s Step) vec3(key string) ([3]float64, bool) {
	arr, ok := s.Raw[key].([]interface{})
	if !ok || len(arr) != 3 {
		return [3]float64{}, false
	}
	var out [3]float64
	for i, v := range arr {
		f, ok := v.(float64)
		if !ok {
			return [3]float64{}, false
		}
		out[i] = f
	}
	return out, true
}

// Typed accessors used by the permission/z3 stages.

func (s Step) ShellCommand() (string, bool)    { return s.str("command") }
func (s Step) FilePath() (string, bool)        { return s.str("path") }
func (s Step) FileWriteContent() (string, bool) { return s.str("content") }
func (s Step) NetworkURL() (string, bool)      { return s.str("url") }
func (s Step) NetworkMethod() string           { return s.strDefault("method", "GET") }
func (s Step) MCPServer() (string, bool)       { return s.str("server") }
func (s Step) MCPTool() (string, bool)         { return s.str("tool") }
func (s Step) AgenticWorkspace() (string, bool) { return s.str("workspace") }
func (s Step) AgenticTools() []string          { return s.stringSlice("tools") }
func (s Step) PreferredModel() (string, bool)  { return s.str("preferred_model") }

func (s Step) JointTargets() map[string]float64 {
	m, ok := s.Raw["joint_targets"].(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	return out
}

func (s Step) DurationS() float64 {
	if f, ok := s.float("duration_s"); ok {
		return f
	}
	return 1.0
}

func (s Step) VelocityScale() float64 {
	if f, ok := s.float("velocity_scale"); ok {
		return f
	}
	return 1.0
}

func (s Step) TargetPosition() ([3]float64, bool) { return s.vec3("target_position") }
func (s Step) TargetPose() ([3]float64, bool)     { return s.vec3("target_pose") }

func (s Step) SkillID() (string, bool) { return s.str("skill_id") }
func (s Step) ContractEnvelope() (*Envelope, bool) {
	raw, ok := s.Raw["contract_envelope"]
	if !ok || raw == nil {
		return nil, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, false
	}
	return &env, true
}

// --- ActionPlan -----------------------------------------------------------------

type ActionPlan struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Task   string `json:"task"`
	Steps  []Step `json:"steps"`
}

// --- Wire-protocol case / verdict shapes ----------------------------------------

// Case is the union of the two corpus case kinds (docs/spec/conformance.md).
// "expect" is deliberately not modeled — the client never needs it, only the
// differential runner does.
type Case struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	V       int             `json:"v"`
	Plan    json.RawMessage `json:"plan"`
	Envelope json.RawMessage `json:"envelope"`
	Options CaseOptions     `json:"options"`
	Command string          `json:"command"`
}

type CaseOptions struct {
	Strict      *bool `json:"strict"`
	Z3TimeoutMs *int  `json:"z3_timeout_ms"`
}

func (o CaseOptions) Z3Timeout() int {
	if o.Z3TimeoutMs != nil {
		return *o.Z3TimeoutMs
	}
	return 500
}

// Violation mirrors opendaisugi.models.Violation for the fields the wire
// protocol treats as normative: Stage and the "step" key of Detail.
type Violation struct {
	Stage  string
	Step   string // "" means null/plan-level
	HasStep bool
}

func V(stage string) Violation                { return Violation{Stage: stage} }
func VStep(stage, step string) Violation       { return Violation{Stage: stage, Step: step, HasStep: true} }

// VerifyVerdict / DecomposeVerdict are what conform emits on stdout.

type VerifyVerdict struct {
	ID         string           `json:"id"`
	OK         bool             `json:"ok"`
	Violations []ViolationWire  `json:"violations"`
}

type ViolationWire struct {
	Stage string  `json:"stage"`
	Step  *string `json:"step"`
}

type DecomposeVerdict struct {
	ID       string   `json:"id"`
	OK       bool     `json:"ok"`
	Heads    []string `json:"heads,omitempty"`
	Commands []string `json:"commands,omitempty"`
	Reads    []string `json:"reads,omitempty"`
	Writes   []string `json:"writes,omitempty"`
}

type ErrorVerdict struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

func ToWireViolations(vs []Violation) []ViolationWire {
	out := make([]ViolationWire, 0, len(vs))
	for _, v := range vs {
		vv := ViolationWire{Stage: v.Stage}
		if v.HasStep {
			s := v.Step
			vv.Step = &s
		}
		out = append(out, vv)
	}
	return out
}

// ParsePlan/ParseEnvelope give callers a typed error including which field
// failed, since a corpus of 13k cases makes "invalid character" alone useless.
func ParsePlan(raw json.RawMessage) (ActionPlan, error) {
	var p ActionPlan
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("plan: %w", err)
	}
	return p, nil
}

func ParseEnvelope(raw json.RawMessage) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, fmt.Errorf("envelope: %w", err)
	}
	return e, nil
}
