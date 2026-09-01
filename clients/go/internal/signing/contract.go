package signing

import (
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
)

func none() any { return nil }

// ContractModel is contracts.Contract.
var ContractModel = &pmodel.Model{Name: "Contract", Fields: []pmodel.Field{
	{Name: "contract_id", Schema: pmodel.Str{}, Required: true},
	{Name: "skill_id", Schema: pmodel.Str{}, Required: true},
	{Name: "version", Schema: pmodel.Str{}, Default: func() any { return "0.1.0" }},
	{Name: "envelope", Schema: pmodel.Envelope, Required: true},
	{Name: "input_schema", Schema: pmodel.Dict{Val: pmodel.Any{}}, Default: func() any { return pyjson.NewObject() }},
	{Name: "output_schema", Schema: pmodel.Dict{Val: pmodel.Any{}}, Default: func() any { return pyjson.NewObject() }},
	{Name: "guarantees", Schema: pmodel.List{Elem: pmodel.Str{}}, Default: func() any { return []any{} }},
	{Name: "created_at", Schema: pmodel.Nullable{Inner: pmodel.Str{}}, Default: none},
	{Name: "signature", Schema: pmodel.Nullable{Inner: pmodel.Str{}}, Default: none},
	{Name: "signer", Schema: pmodel.Nullable{Inner: pmodel.Str{}}, Default: none},
}}

// jsonMode is model_dump(mode="json") of a validated dump: NaN and the
// infinities as None.
func jsonMode(v any) any {
	switch x := v.(type) {
	case float64:
		if x != x || x > 1.7976931348623157e308 || x < -1.7976931348623157e308 {
			return nil
		}
	case pyjson.Float:
		return jsonMode(float64(x))
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonMode(e)
		}
		return out
	case *pyjson.Object:
		out := pyjson.NewObject()
		for _, k := range x.Keys() {
			out.Set(k, jsonMode(x.Value(k)))
		}
		return out
	}
	return v
}

// CanonicalContract is canonicalize_contract: the contract's JSON dump
// without its signature and signer, as sorted compact ASCII JSON.
func CanonicalContract(c *pyjson.Object) []byte {
	body := pyjson.NewObject()
	for _, k := range c.Keys() {
		if k != "signature" && k != "signer" {
			body.Set(k, jsonMode(c.Value(k)))
		}
	}
	return []byte(pyjson.CanonicalASCII(body))
}

// SignContract is sign_contract.
func SignContract(c *pyjson.Object, privB64 string) (string, *PyError) {
	return SignBytes(CanonicalContract(c), privB64)
}

// VerifyContract is verify_signature_raw: false for a contract with no
// signature, and on any failure.
func VerifyContract(c *pyjson.Object, pub any) bool {
	sig, ok := c.Value("signature").(string)
	if !ok {
		return false
	}
	return VerifyWith(CanonicalContract(c), sig, pub)
}

// VerifyNamed is TrustedSignerRegistry.verify: true when the contract's
// signature verifies under any named key; unknown names are skipped.
func (r *Registry) VerifyNamed(c *pyjson.Object, names []string) bool {
	if _, ok := c.Value("signature").(string); !ok {
		return false
	}
	for _, n := range names {
		pub, ok := r.Entries.Get(n)
		if !ok || pub == nil {
			continue
		}
		if VerifyContract(c, pub) {
			return true
		}
	}
	return false
}
