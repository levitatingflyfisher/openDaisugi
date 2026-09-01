// Package bundle is the oracle's pathway bundle (opendaisugi/pathway_bundle.py):
// a compiled pathway as a content-addressed, ed25519-signed unit that
// travels between instances through a git registry.
//
// Bundles and pathways are model_dump(mode="json") values (*pyjson.Object),
// so the bytes hashed and signed are the oracle's bytes.
package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"daisugi-verify/internal/pathways"
	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
	"daisugi-verify/internal/signing"
	"daisugi-verify/internal/tracejournal"
)

func none() any { return nil }

// Model is pathway_bundle.PathwayBundle. The oracle keeps extra fields
// (extra="allow"); nothing this binary does reads them.
var Model = &pmodel.Model{Name: "PathwayBundle", Fields: []pmodel.Field{
	{Name: "bundle_format_version", Schema: pmodel.Int{}, Default: func() any { return pyjson.Int{Text: "1"} }},
	{Name: "pathway", Schema: pmodel.CompiledPathway, Required: true},
	{Name: "structure_signature", Schema: pmodel.Str{}, Required: true},
	{Name: "publisher", Schema: pmodel.Str{}, Required: true},
	{Name: "published_at", Schema: pmodel.Float{}, Required: true},
	{Name: "bundle_hash", Schema: pmodel.Str{}, Required: true},
	{Name: "signature_b64", Schema: pmodel.Nullable{Inner: pmodel.Str{}}, Default: none},
	{Name: "signer_pubkey_b64", Schema: pmodel.Nullable{Inner: pmodel.Str{}}, Default: none},
}}

// CanonicalPayload is _canonical_payload: the sorted, compact, ASCII JSON
// of the pathway's JSON dump, the publisher and the publication time,
// which is hashed and signed. publishedAt is written as the caller holds
// it (an int stays an int, as in Python).
func CanonicalPayload(pathway *pyjson.Object, publisher string, publishedAt any) []byte {
	body := pyjson.NewObject().
		Set("pathway", pathways.JSONMode(pathway)).
		Set("publisher", publisher).
		Set("published_at", publishedAt)
	return []byte(pyjson.CanonicalASCII(body))
}

// Hash is compute_bundle_hash.
func Hash(pathway *pyjson.Object, publisher string, publishedAt any) string {
	sum := sha256.Sum256(CanonicalPayload(pathway, publisher, publishedAt))
	return hex.EncodeToString(sum[:])
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case pyjson.Float:
		return float64(x)
	case pyjson.Int:
		f, _ := pmodel.FloatFromString(x.Text)
		return f
	case int:
		return float64(x)
	}
	return 0
}

// ToBundle is pathway_to_bundle: the bundle's model dump (mode="json").
// pathway is a validated CompiledPathway dump. priv and pub are optional;
// a private key with no public key is the oracle's ValueError.
func ToBundle(pathway *pyjson.Object, publisher string, publishedAt any, priv, pub *string) (*pyjson.Object, *signing.PyError) {
	sig := ""
	if s, _ := pathway.Value("structure_signature").(string); s != "" {
		sig = s
	} else if tpl, ok := pathway.Value("plan_template").(*pyjson.Object); ok {
		if s, ok := tracejournal.StructureSignature(tpl); ok {
			sig = s
		}
	}
	hash := Hash(pathway, publisher, publishedAt)
	var sigB64 any
	if priv != nil {
		if pub == nil {
			return nil, &signing.PyError{Type: "ValueError", Msg: "pathway_to_bundle: signing requires both private_key_b64 " +
				"and public_key_b64 (the latter goes into the bundle for consumer verification)"}
		}
		s, err := signing.SignBytes(CanonicalPayload(pathway, publisher, publishedAt), *priv)
		if err != nil {
			return nil, err
		}
		sigB64 = s
	}
	var signer any
	if sigB64 != nil {
		signer = *pub
	}
	return pyjson.NewObject().
		Set("bundle_format_version", pyjson.Int{Text: "1"}).
		Set("pathway", pathways.JSONMode(pathway)).
		Set("structure_signature", sig).
		Set("publisher", publisher).
		Set("published_at", asFloat(publishedAt)).
		Set("bundle_hash", hash).
		Set("signature_b64", sigB64).
		Set("signer_pubkey_b64", signer), nil
}

func short(s string, n int) string {
	r := pystr.Runes(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

const errModule = "opendaisugi.pathway_bundle."

// FromBundle is bundle_to_pathway on a validated bundle dump: the
// pathway, or the oracle's refusal. trusted nil is None (no trust set).
func FromBundle(b *pyjson.Object, trusted []string, requireSigned bool) (*pyjson.Object, *signing.PyError) {
	hash, _ := b.Value("bundle_hash").(string)
	sig, signed := b.Value("signature_b64").(string)
	if !signed {
		if requireSigned {
			return nil, &signing.PyError{Type: errModule + "UnsignedBundleError",
				Msg: fmt.Sprintf("bundle %s is unsigned; refusing", short(hash, 12))}
		}
		return b.Value("pathway").(*pyjson.Object), nil
	}
	pub, hasPub := b.Value("signer_pubkey_b64").(string)
	if !hasPub {
		return nil, &signing.PyError{Type: errModule + "InvalidSignatureError",
			Msg: fmt.Sprintf("bundle %s carries a signature but no signer pubkey to verify against", short(hash, 12))}
	}
	if trusted == nil {
		return nil, &signing.PyError{Type: errModule + "UntrustedSignerError",
			Msg: fmt.Sprintf("bundle %s is signed but no trusted_pubkey_b64s was supplied to verify against; "+
				"refusing (a self-supplied key proves nothing)", short(hash, 12))}
	}
	found := false
	for _, t := range trusted {
		if t == pub {
			found = true
			break
		}
	}
	if !found {
		return nil, &signing.PyError{Type: errModule + "UntrustedSignerError",
			Msg: fmt.Sprintf("bundle %s signed by %s…; not in trusted-signers list", short(hash, 12), short(pub, 16))}
	}
	pw := b.Value("pathway").(*pyjson.Object)
	payload := CanonicalPayload(pw, b.Value("publisher").(string), b.Value("published_at"))
	if !signing.VerifyBytes(payload, sig, pub) {
		return nil, &signing.PyError{Type: errModule + "InvalidSignatureError",
			Msg: fmt.Sprintf("bundle %s signature does not verify", short(hash, 12))}
	}
	return pw, nil
}

// Validate is PathwayBundle.model_validate(v).
func Validate(v any) (*pyjson.Object, *pmodel.ValidationError) {
	out, err := pmodel.Validate("PathwayBundle", Model, v, pmodel.Python)
	if err != nil {
		return nil, err
	}
	return out.(*pyjson.Object), nil
}
