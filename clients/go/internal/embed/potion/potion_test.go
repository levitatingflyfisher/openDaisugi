package potion

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The fixtures are written by clients/pathway_cases.py from the oracle:
// potion-tiny is a small model in model2vec's file format, and units.json
// holds its token ids and vectors for a corpus of texts.
const (
	fixtures = "../../../../fixtures/pathways"
	tinyDir  = fixtures + "/potion-tiny"
)

type units struct {
	Tokens []struct {
		Text string `json:"text"`
		IDs  []int  `json:"ids"`
	} `json:"tiny_tokens"`
	Vectors []struct {
		Text string    `json:"text"`
		Vec  []float64 `json:"vec"`
	} `json:"tiny_vectors"`
}

func load(t *testing.T) units {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, "units.json"))
	if err != nil {
		t.Fatal(err)
	}
	var u units
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestTinyTokenIDsMatchTokenizers(t *testing.T) {
	u := load(t)
	data, err := os.ReadFile(filepath.Join(tinyDir, "tokenizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ParseTokenizer(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range u.Tokens {
		got := tok.Encode(c.Text)
		if len(got) == 0 && len(c.IDs) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.IDs) {
			t.Errorf("Encode(%q) = %v, tokenizers %v", c.Text, got, c.IDs)
		}
	}
}

func TestTinyVectorsMatchModel2vec(t *testing.T) {
	u := load(t)
	m, err := LoadDir(tinyDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range u.Vectors {
		got := m.Encode(c.Text)
		if len(got) != len(c.Vec) {
			t.Fatalf("width %d, oracle %d", len(got), len(c.Vec))
		}
		for i := range got {
			if math.Abs(got[i]-c.Vec[i]) > 1e-6 {
				t.Errorf("Encode(%q)[%d] = %v, oracle %v", c.Text, i, got[i], c.Vec[i])
				break
			}
		}
	}
}

func TestOpenLocalDirAndRefusals(t *testing.T) {
	e := Env{Lookup: func(string) (string, bool) { return "", false }, Home: t.TempDir(), NoFetch: true}
	if _, err := Open(tinyDir, e); err != nil {
		t.Fatalf("a local model directory: %v", err)
	}
	for _, id := range []string{"someone/other-model", filepath.Join(t.TempDir(), "absent"), ""} {
		if _, err := Open(id, e); !errors.Is(err, ErrNotAvailable) {
			t.Errorf("Open(%q) = %v, want ErrNotAvailable", id, err)
		}
	}
	// The pinned default with an empty cache and no fetch allowed: not
	// available, and nothing written.
	if _, err := Open(DefaultModel, e); !errors.Is(err, ErrNotAvailable) {
		t.Errorf("the default model with no cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Home, ".cache")); !os.IsNotExist(err) {
		t.Errorf("a refused fetch wrote the cache directory")
	}
}

func TestFetchChecksThePin(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "config.json")
	if err := os.WriteFile(dest, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := Env{NoFetch: true}
	// A file that does not match its pin is never used as it is.
	if err := fetch("http://127.0.0.1:0/none", pinnedFiles[0], dest, e); err == nil {
		t.Fatal("a file with the wrong sha256 was accepted")
	}
}

func TestResolveModel(t *testing.T) {
	none := func(string) (string, bool) { return "", false }
	if got := ResolveModel(none); got != DefaultModel {
		t.Errorf("unset: %q", got)
	}
	set := func(k string) (string, bool) { return "/models/x", k == "OPENDAISUGI_POTION_MODEL" }
	if got := ResolveModel(set); got != "/models/x" {
		t.Errorf("set: %q", got)
	}
	empty := func(k string) (string, bool) { return "", k == "OPENDAISUGI_POTION_MODEL" }
	if got := ResolveModel(empty); got != "" {
		t.Errorf("set empty: %q (os.environ.get keeps an empty value)", got)
	}
}

func TestRefusesUnmodelledTokenizers(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(tinyDir, "tokenizer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(map[string]any){
		"padding":    func(m map[string]any) { m["padding"] = map[string]any{"strategy": "BatchLongest"} },
		"normalizer": func(m map[string]any) { m["normalizer"] = map[string]any{"type": "NFC"} },
		"pre":        func(m map[string]any) { m["pre_tokenizer"] = map[string]any{"type": "Whitespace"} },
		"model":      func(m map[string]any) { m["model"].(map[string]any)["type"] = "BPE" },
	} {
		m := map[string]any{}
		for k, v := range raw {
			m[k] = v
		}
		mc := map[string]any{}
		for k, v := range raw["model"].(map[string]any) {
			mc[k] = v
		}
		m["model"] = mc
		edit(m)
		b, _ := json.Marshal(m)
		if _, err := ParseTokenizer(b); err == nil {
			t.Errorf("%s: an unmodelled tokenizer was accepted", name)
		}
	}
}

func TestCraftedSafetensorsShapesAreErrorsNotPanics(t *testing.T) {
	for _, header := range []string{
		`{"embeddings":{"dtype":"F32","shape":[4611686018427387904,4],"data_offsets":[0,16]}}`,
		`{"embeddings":{"dtype":"F32","shape":[-1,-4],"data_offsets":[0,16]}}`,
		`{"embeddings":{"dtype":"F32","shape":[2,3],"data_offsets":[0,16]}}`,
		`{"embeddings":{"dtype":"F32","shape":[0,4],"data_offsets":[0,16]}}`,
		`{"embeddings":{"dtype":"I8","shape":[4,4],"data_offsets":[0,16]}}`,
	} {
		b := make([]byte, 8, 8+len(header)+16)
		binary.LittleEndian.PutUint64(b, uint64(len(header)))
		b = append(b, header...)
		b = append(b, make([]byte, 16)...)
		cfg := []byte(`{"normalize": true}`)
		tok, _ := os.ReadFile(filepath.Join(tinyDir, "tokenizer.json"))
		if _, err := Load(cfg, tok, b); err == nil {
			t.Errorf("header %s loaded", header)
		}
	}
}
