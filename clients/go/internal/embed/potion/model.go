// Package potion is the model2vec matcher of ADR-0018 (potion-base-8M by
// default): a static embedding table, one row per token, mean-pooled.
// It is model2vec's StaticModel.encode, with the tokenizer ported in
// tokenizer.go, and it runs with no Python and no tokenizers library.
package potion

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"
)

// Model is a loaded StaticModel.
type Model struct {
	tok       *Tokenizer
	emb       []float32 // rows x dim
	rows, dim int
	weights   []float32 // nil, or one weight per token id
	mapping   []int64   // nil, or token id -> row
	normalize bool
	medianLen int
}

// Dim is the width of a vector.
func (m *Model) Dim() int { return m.dim }

// LoadDir reads a model2vec directory: config.json, model.safetensors
// and tokenizer.json.
func LoadDir(dir string) (*Model, error) {
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
	cfgData, err := read("config.json")
	if err != nil {
		return nil, err
	}
	tokData, err := read("tokenizer.json")
	if err != nil {
		return nil, err
	}
	stData, err := read("model.safetensors")
	if err != nil {
		return nil, err
	}
	return Load(cfgData, tokData, stData)
}

// Load builds a model from the bytes of its three files.
func Load(cfgData, tokData, stData []byte) (*Model, error) {
	var cfg map[string]any
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return nil, fmt.Errorf("config.json: %v", err)
	}
	tok, err := ParseTokenizer(tokData)
	if err != nil {
		return nil, err
	}
	tensors, err := readSafetensors(stData)
	if err != nil {
		return nil, err
	}
	e, ok := tensors["embeddings"]
	if !ok || len(e.shape) != 2 {
		return nil, errors.New("model.safetensors has no 2-D 'embeddings' tensor")
	}
	if e.shape[0] < 0 || e.shape[1] <= 0 {
		return nil, errors.New("model.safetensors: the embeddings tensor has an empty shape")
	}
	m := &Model{tok: tok, rows: e.shape[0], dim: e.shape[1]}
	if m.emb, err = e.floats(); err != nil {
		return nil, err
	}
	if w, ok := tensors["weights"]; ok {
		if m.weights, err = w.floats(); err != nil {
			return nil, err
		}
	}
	if mp, ok := tensors["mapping"]; ok {
		if m.mapping, err = mp.ints(); err != nil {
			return nil, err
		}
	}
	// StaticModel.__init__: the vocabulary sorted by id, one vector each
	// unless a mapping is given.
	vocab := tok.Vocab()
	type entry struct {
		tok string
		id  int
	}
	entries := make([]entry, 0, len(vocab))
	for k, v := range vocab {
		entries = append(entries, entry{k, v})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	if m.mapping == nil && len(entries) != m.rows {
		return nil, fmt.Errorf("number of tokens (%d) does not match number of vectors (%d)", len(entries), m.rows)
	}
	lens := make([]int, len(entries))
	for i, en := range entries {
		lens[i] = utf8.RuneCountInString(en.tok)
	}
	sort.Ints(lens)
	if n := len(lens); n > 0 {
		if n%2 == 1 {
			m.medianLen = lens[n/2]
		} else {
			m.medianLen = int((float64(lens[n/2-1]) + float64(lens[n/2])) / 2)
		}
	}
	maxID := -1
	for _, en := range entries {
		if en.id > maxID {
			maxID = en.id
		}
	}
	if m.mapping != nil {
		if len(m.mapping) <= maxID {
			return nil, errors.New("model.safetensors: the token mapping is shorter than the vocabulary")
		}
		for _, r := range m.mapping {
			if r < 0 || int(r) >= m.rows {
				return nil, errors.New("model.safetensors: the token mapping names a row past the table")
			}
		}
	} else if maxID >= m.rows {
		return nil, errors.New("model.safetensors: a token id is past the table")
	}
	if m.weights != nil && len(m.weights) <= maxID {
		return nil, errors.New("model.safetensors: the weights are shorter than the vocabulary")
	}
	if b, ok := cfg["normalize"].(bool); ok {
		m.normalize = b
	}
	return m, nil
}

// maxLength is encode's default max_length.
const maxLength = 512

// Encode is StaticModel.encode([text])[0] followed by the oracle's own
// L2 normalization (_search._l2).
func (m *Model) Encode(text string) []float64 {
	// tokenize(): the text cut to max_length * median token length code
	// points, [UNK] dropped, then max_length ids.
	if limit := maxLength * m.medianLen; utf8.RuneCountInString(text) > limit {
		n := 0
		for i := range text {
			if n == limit {
				text = text[:i]
				break
			}
			n++
		}
	}
	var ids []int
	for _, id := range m.tok.Encode(text) {
		if id != m.tok.unkID {
			ids = append(ids, id)
		}
	}
	if len(ids) > maxLength {
		ids = ids[:maxLength]
	}
	out := make([]float64, m.dim)
	if len(ids) > 0 {
		for _, id := range ids {
			row := id
			if m.mapping != nil {
				row = int(m.mapping[id])
			}
			w := 1.0
			if m.weights != nil {
				w = float64(m.weights[id])
			}
			base := row * m.dim
			for j := 0; j < m.dim; j++ {
				out[j] += float64(m.emb[base+j]) * w
			}
		}
		for j := range out {
			out[j] /= float64(len(ids))
		}
		if m.normalize {
			n := norm(out) + 1e-32
			for j := range out {
				out[j] /= n
			}
		}
	}
	n := math.Max(norm(out), 1e-12)
	for j := range out {
		out[j] /= n
	}
	return out
}

func norm(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return math.Sqrt(s)
}

type tensor struct {
	dtype string
	shape []int
	data  []byte
}

// count is the number of elements, or an error for a shape whose product
// is negative, overflows, or does not match the tensor's bytes at width w.
func (t tensor) count(w int) (int, error) {
	n := 1
	for _, d := range t.shape {
		if d < 0 || (d > 0 && n > len(t.data)/d) {
			return 0, errors.New("model.safetensors: a tensor's shape does not match its size")
		}
		n *= d
	}
	if n*w != len(t.data) {
		return 0, errors.New("model.safetensors: a tensor's size does not match its shape")
	}
	return n, nil
}

func (t tensor) width() int {
	switch t.dtype {
	case "F64", "I64":
		return 8
	case "F32", "I32":
		return 4
	case "F16":
		return 2
	}
	return 0
}

func (t tensor) floats() ([]float32, error) {
	if t.width() == 0 || t.dtype == "I64" || t.dtype == "I32" {
		return nil, fmt.Errorf("model.safetensors: dtype %s is not read by this binary", t.dtype)
	}
	n, err := t.count(t.width())
	if err != nil {
		return nil, err
	}
	out := make([]float32, n)
	switch t.dtype {
	case "F32":
		if len(t.data) != 4*n {
			return nil, errors.New("model.safetensors: a tensor's size does not match its shape")
		}
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(t.data[4*i:]))
		}
	case "F16":
		if len(t.data) != 2*n {
			return nil, errors.New("model.safetensors: a tensor's size does not match its shape")
		}
		for i := range out {
			out[i] = half(binary.LittleEndian.Uint16(t.data[2*i:]))
		}
	case "F64":
		if len(t.data) != 8*n {
			return nil, errors.New("model.safetensors: a tensor's size does not match its shape")
		}
		for i := range out {
			// model2vec keeps the table's own dtype; float64 tables are
			// narrowed here, a difference far below the score tolerance.
			out[i] = float32(math.Float64frombits(binary.LittleEndian.Uint64(t.data[8*i:])))
		}
	default:
		return nil, fmt.Errorf("model.safetensors: dtype %s is not read by this binary", t.dtype)
	}
	return out, nil
}

func (t tensor) ints() ([]int64, error) {
	if t.dtype != "I64" && t.dtype != "I32" {
		return nil, fmt.Errorf("model.safetensors: dtype %s is not read by this binary", t.dtype)
	}
	n, err := t.count(t.width())
	if err != nil {
		return nil, err
	}
	out := make([]int64, n)
	switch t.dtype {
	case "I64":
		if len(t.data) != 8*n {
			return nil, errors.New("model.safetensors: a tensor's size does not match its shape")
		}
		for i := range out {
			out[i] = int64(binary.LittleEndian.Uint64(t.data[8*i:]))
		}
	case "I32":
		if len(t.data) != 4*n {
			return nil, errors.New("model.safetensors: a tensor's size does not match its shape")
		}
		for i := range out {
			out[i] = int64(int32(binary.LittleEndian.Uint32(t.data[4*i:])))
		}
	default:
		return nil, fmt.Errorf("model.safetensors: dtype %s is not read by this binary", t.dtype)
	}
	return out, nil
}

func half(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	frac := uint32(h) & 0x3ff
	switch {
	case exp == 0:
		f := float32(frac) / 1024 * float32(math.Pow(2, -14))
		if sign != 0 {
			return -f
		}
		return f
	case exp == 31:
		return math.Float32frombits(sign | 0x7f800000 | frac<<13)
	}
	return math.Float32frombits(sign | (exp+112)<<23 | frac<<13)
}

// readSafetensors reads the safetensors format: an 8-byte little-endian
// header length, a JSON header, then the tensor bytes.
func readSafetensors(b []byte) (map[string]tensor, error) {
	if len(b) < 8 {
		return nil, errors.New("model.safetensors is too short")
	}
	n := binary.LittleEndian.Uint64(b)
	if n > uint64(len(b)-8) {
		return nil, errors.New("model.safetensors: the header runs past the file")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(b[8:8+n], &header); err != nil {
		return nil, fmt.Errorf("model.safetensors header: %v", err)
	}
	data := b[8+n:]
	out := map[string]tensor{}
	for name, raw := range header {
		if name == "__metadata__" {
			continue
		}
		var h struct {
			Dtype   string   `json:"dtype"`
			Shape   []int    `json:"shape"`
			Offsets []uint64 `json:"data_offsets"`
		}
		if err := json.Unmarshal(raw, &h); err != nil || len(h.Offsets) != 2 {
			return nil, fmt.Errorf("model.safetensors: tensor %q has a bad header", name)
		}
		if h.Offsets[0] > h.Offsets[1] || h.Offsets[1] > uint64(len(data)) {
			return nil, fmt.Errorf("model.safetensors: tensor %q runs past the file", name)
		}
		out[name] = tensor{dtype: h.Dtype, shape: h.Shape, data: data[h.Offsets[0]:h.Offsets[1]]}
	}
	return out, nil
}
