package verify

import (
	"encoding/json"
	"fmt"
)

// Expression mirrors the predicate.py discriminated union. Every concrete
// type below implements it; Op returns the JSON "op" discriminator.
type Expression interface {
	Op() string
}

type Equals struct {
	Path  string
	Value interface{}
}

func (Equals) Op() string { return "equals" }

type NotEquals struct {
	Path  string
	Value interface{}
}

func (NotEquals) Op() string { return "not_equals" }

type InSet struct {
	Path   string
	Values []interface{}
}

func (InSet) Op() string { return "in_set" }

type NotInSet struct {
	Path   string
	Values []interface{}
}

func (NotInSet) Op() string { return "not_in_set" }

type Matches struct {
	Path  string
	Regex string
}

func (Matches) Op() string { return "matches" }

type NotMatches struct {
	Path  string
	Regex string
}

func (NotMatches) Op() string { return "not_matches" }

type NumericRange struct {
	Path     string
	Min, Max float64
}

func (NumericRange) Op() string { return "numeric_range" }

type LengthRange struct {
	Path string
	Min  int
	Max  *int
}

func (LengthRange) Op() string { return "length_range" }

type Exists struct{ Path string }

func (Exists) Op() string { return "exists" }

type IsEmpty struct{ Path string }

func (IsEmpty) Op() string { return "is_empty" }

type And struct{ Children []Expression }

func (And) Op() string { return "and" }

type Or struct{ Children []Expression }

func (Or) Op() string { return "or" }

type Not struct{ Child Expression }

func (Not) Op() string { return "not" }

type Implies struct{ A, B Expression }

func (Implies) Op() string { return "implies" }

type ForallSteps struct{ Pred Expression }

func (ForallSteps) Op() string { return "forall_steps" }

type ExistsStep struct{ Pred Expression }

func (ExistsStep) Op() string { return "exists_step" }

type ForallOutputs struct{ Pred Expression }

func (ForallOutputs) Op() string { return "forall_outputs" }

type DependsOn struct{ StepIDA, StepIDB string }

func (DependsOn) Op() string { return "depends_on" }

type Before struct{ StepIDA, StepIDB string }

func (Before) Op() string { return "before" }

type AliasRef struct {
	Name string
	Args map[string]interface{}
}

func (AliasRef) Op() string { return "alias" }

type LLMCheck struct{ Rule string }

func (LLMCheck) Op() string { return "llm_check" }

// exprJSON is the union of every field any Expression variant might carry —
// parsed once, then dispatched on Op.
type exprJSON struct {
	Op        string            `json:"op"`
	Path      *string           `json:"path"`
	Value     json.RawMessage   `json:"value"`
	Values    []json.RawMessage `json:"values"`
	Regex     *string           `json:"regex"`
	Min       *float64          `json:"min"`
	Max       *float64          `json:"max"`
	Children  []json.RawMessage `json:"children"`
	Child     json.RawMessage   `json:"child"`
	A         json.RawMessage   `json:"a"`
	B         json.RawMessage   `json:"b"`
	Pred      json.RawMessage   `json:"pred"`
	StepIDA   *string           `json:"step_id_a"`
	StepIDB   *string           `json:"step_id_b"`
	Name      *string           `json:"name"`
	Args      map[string]interface{} `json:"args"`
	Rule      *string           `json:"rule"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ParseExpression ports predicate.parse_expression: decode a raw JSON
// predicate-algebra node into a typed Expression tree.
func ParseExpression(raw json.RawMessage) (Expression, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("empty expression")
	}
	var e exprJSON
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("predicate expression: %w", err)
	}
	switch e.Op {
	case "equals":
		v, err := decodeAny(e.Value)
		return Equals{Path: str(e.Path), Value: v}, err
	case "not_equals":
		v, err := decodeAny(e.Value)
		return NotEquals{Path: str(e.Path), Value: v}, err
	case "in_set":
		vs, err := decodeAnySlice(e.Values)
		return InSet{Path: str(e.Path), Values: vs}, err
	case "not_in_set":
		vs, err := decodeAnySlice(e.Values)
		return NotInSet{Path: str(e.Path), Values: vs}, err
	case "matches":
		return Matches{Path: str(e.Path), Regex: str(e.Regex)}, nil
	case "not_matches":
		return NotMatches{Path: str(e.Path), Regex: str(e.Regex)}, nil
	case "numeric_range":
		min, max := 0.0, 0.0
		if e.Min != nil {
			min = *e.Min
		}
		if e.Max != nil {
			max = *e.Max
		}
		return NumericRange{Path: str(e.Path), Min: min, Max: max}, nil
	case "length_range":
		min := 0
		if e.Min != nil {
			min = int(*e.Min)
		}
		var max *int
		if e.Max != nil {
			m := int(*e.Max)
			max = &m
		}
		return LengthRange{Path: str(e.Path), Min: min, Max: max}, nil
	case "exists":
		return Exists{Path: str(e.Path)}, nil
	case "is_empty":
		return IsEmpty{Path: str(e.Path)}, nil
	case "and":
		children, err := decodeExprSlice(e.Children)
		return And{Children: children}, err
	case "or":
		children, err := decodeExprSlice(e.Children)
		return Or{Children: children}, err
	case "not":
		child, err := ParseExpression(e.Child)
		return Not{Child: child}, err
	case "implies":
		a, err := ParseExpression(e.A)
		if err != nil {
			return nil, err
		}
		b, err := ParseExpression(e.B)
		return Implies{A: a, B: b}, err
	case "forall_steps":
		pred, err := ParseExpression(e.Pred)
		return ForallSteps{Pred: pred}, err
	case "exists_step":
		pred, err := ParseExpression(e.Pred)
		return ExistsStep{Pred: pred}, err
	case "forall_outputs":
		pred, err := ParseExpression(e.Pred)
		return ForallOutputs{Pred: pred}, err
	case "depends_on":
		return DependsOn{StepIDA: str(e.StepIDA), StepIDB: str(e.StepIDB)}, nil
	case "before":
		return Before{StepIDA: str(e.StepIDA), StepIDB: str(e.StepIDB)}, nil
	case "alias":
		args := e.Args
		if args == nil {
			args = map[string]interface{}{}
		}
		return AliasRef{Name: str(e.Name), Args: args}, nil
	case "llm_check":
		return LLMCheck{Rule: str(e.Rule)}, nil
	default:
		return nil, fmt.Errorf("unknown predicate op %q", e.Op)
	}
}

func decodeAny(raw json.RawMessage) (interface{}, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func decodeAnySlice(raws []json.RawMessage) ([]interface{}, error) {
	out := make([]interface{}, 0, len(raws))
	for _, r := range raws {
		v, err := decodeAny(r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func decodeExprSlice(raws []json.RawMessage) ([]Expression, error) {
	out := make([]Expression, 0, len(raws))
	for _, r := range raws {
		e, err := ParseExpression(r)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
