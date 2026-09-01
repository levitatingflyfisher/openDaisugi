package bundle

import (
	"strings"
	"testing"

	"daisugi-verify/internal/pmodel"
	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/signing"
)

func pathway(t *testing.T) *pyjson.Object {
	v, err := pyjson.Loads(`{"id": "pw_1", "task_description": "deploy", "task_embedding": [0.5, 0.0],
		"envelope": {"id": "env_00000001", "generated_by": "t", "task": "t", "permissions": {}},
		"plan_template": {"id": "plan_00000001", "source": "script", "task": "t",
			"steps": [{"id": "s1", "type": "shell", "command": "make"}]},
		"source_trace_ids": [], "distilled_at": 1.5}`)
	if err != nil {
		t.Fatal(err)
	}
	out, verr := pmodel.Validate("CompiledPathway", pmodel.CompiledPathway, v, pmodel.Python)
	if verr != nil {
		t.Fatal(verr)
	}
	return out.(*pyjson.Object)
}

// A signed bundle comes back only under a trust set that names its key,
// and not once a signed field changes.
func TestASignedBundleComesBackOnlyUnderTrust(t *testing.T) {
	priv, pub, err := signing.GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	p := pathway(t)
	b, perr := ToBundle(p, "team", 1600000000.25, &priv, &pub)
	if perr != nil {
		t.Fatal(perr)
	}
	if b.Value("bundle_hash") != Hash(p, "team", 1600000000.25) || b.Value("structure_signature") != "shell" {
		t.Fatalf("bundle %s", pyjson.Dumps(b, true))
	}
	// The bundle as a file holds it: its JSON read back.
	raw, err := pyjson.Loads(pyjson.Dumps(b, false))
	if err != nil {
		t.Fatal(err)
	}
	vb, verr := Validate(raw)
	if verr != nil {
		t.Fatal(verr)
	}
	if _, perr := FromBundle(vb, []string{pub}, true); perr != nil {
		t.Fatal(perr)
	}
	for _, trusted := range [][]string{nil, {}} {
		if _, perr := FromBundle(vb, trusted, true); perr == nil || !strings.HasSuffix(perr.Type, "UntrustedSignerError") {
			t.Fatalf("trust %v: %v", trusted, perr)
		}
	}
	vb.Set("publisher", "mallory")
	if _, perr := FromBundle(vb, []string{pub}, true); perr == nil || !strings.HasSuffix(perr.Type, "InvalidSignatureError") {
		t.Fatalf("tampered: %v", perr)
	}
}

// An unsigned bundle passes only when signing is not required, and a
// private key with no public key is the oracle's ValueError.
func TestAnUnsignedBundlePassesOnlyWhenNotRequired(t *testing.T) {
	p := pathway(t)
	b, perr := ToBundle(p, "team", pyjson.Int{Text: "7"}, nil, nil)
	if perr != nil {
		t.Fatal(perr)
	}
	if b.Value("signature_b64") != nil || b.Value("published_at") != 7.0 {
		t.Fatalf("bundle %s", pyjson.Dumps(b, true))
	}
	if _, perr := FromBundle(b, nil, true); perr == nil || !strings.HasSuffix(perr.Type, "UnsignedBundleError") {
		t.Fatalf("required: %v", perr)
	}
	if _, perr := FromBundle(b, nil, false); perr != nil {
		t.Fatal(perr)
	}
	k := "x"
	if _, perr := ToBundle(p, "team", 1.0, &k, nil); perr == nil || perr.Type != "ValueError" {
		t.Fatalf("no public key: %v", perr)
	}
}
