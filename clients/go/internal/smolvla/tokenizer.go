// Package smolvla runs lerobot/smolvla_base from its three ONNX graphs:
// the tokenizer, the pre and post processing and the Euler loop that stay
// outside the graphs (ruling VL-R-3), and, with the mujoco build tag, the
// graphs themselves through ONNX Runtime.
package smolvla

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// Width is the checkpoint's tokenizer_max_length: the token ids the prefix
// graph takes.
const Width = 48

// padToken is the backbone's pad_token (tokenizer_config.json).
const padToken = "<|im_end|>"

type rng struct{ lo, hi rune }

func in(table []rng, r rune) bool {
	i := sort.Search(len(table), func(i int) bool { return table[i].hi >= r })
	return i < len(table) && table[i].lo <= r
}

func isLetter(r rune) bool { return in(letterRanges, r) }
func isNumber(r rune) bool { return in(numberRanges, r) }
func isSpace(r rune) bool  { return in(spaceRanges, r) }
func isOther(r rune) bool  { return !isLetter(r) && !isNumber(r) && !isSpace(r) }

// Tokenizer is the minimal byte-level BPE the oracle runs: added tokens
// matched first, then the ByteLevel pre-tokenizer, then BPE merges by
// rank, with the vocabulary, merges and added tokens of the backbone's
// tokenizer.json. The oracle's tokenizer is transformers' GPT2Tokenizer,
// which builds its own pipeline (ByteLevel, no normalizer, no special
// tokens added) and does not run the Digits pre-tokenizer the file also
// names (ruling VL-R-5).
type Tokenizer struct {
	vocab  map[string]int64
	ranks  map[[2]string]int
	added  []addedToken // longest first
	pad    int64
	byteCh [256]rune
}

type addedToken struct {
	content string
	id      int64
}

// LoadTokenizer reads a tokenizer.json.
func LoadTokenizer(path string) (*Tokenizer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseTokenizer(b)
}

// ParseTokenizer parses a tokenizer.json. It refuses any file whose
// pipeline this code does not model, rather than tokenize it wrongly.
func ParseTokenizer(b []byte) (*Tokenizer, error) {
	var f struct {
		AddedTokens []struct {
			ID         int64  `json:"id"`
			Content    string `json:"content"`
			SingleWord bool   `json:"single_word"`
			LStrip     bool   `json:"lstrip"`
			RStrip     bool   `json:"rstrip"`
			Normalized bool   `json:"normalized"`
		} `json:"added_tokens"`
		Model struct {
			Type            string            `json:"type"`
			Vocab           map[string]int64  `json:"vocab"`
			Merges          []json.RawMessage `json:"merges"`
			Dropout         *float64          `json:"dropout"`
			ByteFallback    bool              `json:"byte_fallback"`
			IgnoreMerges    bool              `json:"ignore_merges"`
			ContinuingSub   *string           `json:"continuing_subword_prefix"`
			EndOfWordSuffix *string           `json:"end_of_word_suffix"`
		} `json:"model"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %w", err)
	}
	if f.Model.Type != "BPE" {
		return nil, fmt.Errorf("tokenizer.json: model %q is not BPE", f.Model.Type)
	}
	if f.Model.Dropout != nil || f.Model.ByteFallback || f.Model.IgnoreMerges ||
		f.Model.ContinuingSub != nil || f.Model.EndOfWordSuffix != nil {
		return nil, errors.New("tokenizer.json: a BPE option is not supported")
	}
	t := &Tokenizer{vocab: f.Model.Vocab, ranks: make(map[[2]string]int, len(f.Model.Merges)), pad: -1}
	for i, m := range f.Model.Merges {
		var pair [2]string
		var list []string
		var s string
		if json.Unmarshal(m, &list) == nil && len(list) == 2 {
			pair = [2]string{list[0], list[1]}
		} else if json.Unmarshal(m, &s) == nil && strings.Count(s, " ") == 1 {
			a, c, _ := strings.Cut(s, " ")
			pair = [2]string{a, c}
		} else {
			return nil, fmt.Errorf("tokenizer.json: merge %d is not a pair", i)
		}
		if _, dup := t.ranks[pair]; !dup {
			t.ranks[pair] = i
		}
	}
	for _, a := range f.AddedTokens {
		if a.SingleWord || a.LStrip || a.RStrip || a.Normalized || a.Content == "" {
			return nil, fmt.Errorf("tokenizer.json: added token %q has an option that is not supported", a.Content)
		}
		t.added = append(t.added, addedToken{a.Content, a.ID})
		if a.Content == padToken {
			t.pad = a.ID
		}
	}
	if t.pad < 0 {
		return nil, fmt.Errorf("tokenizer.json: no pad token %s", padToken)
	}
	sort.SliceStable(t.added, func(i, j int) bool { return len(t.added[i].content) > len(t.added[j].content) })
	t.byteCh = byteChars()
	return t, nil
}

// byteChars is GPT-2's bytes_to_unicode: each byte as a printable rune.
func byteChars() [256]rune {
	var out [256]rune
	n := 0
	for b := 0; b < 256; b++ {
		if (b >= '!' && b <= '~') || (b >= 0xA1 && b <= 0xAC) || (b >= 0xAE && b <= 0xFF) {
			out[b] = rune(b)
		} else {
			out[b] = rune(256 + n)
			n++
		}
	}
	return out
}

// Encode is the tokenizer's ids for text, with no special tokens added.
func (t *Tokenizer) Encode(text string) ([]int64, error) {
	var ids []int64
	start := 0
	for i := 0; i < len(text); {
		if a, ok := t.addedAt(text[i:]); ok {
			if err := t.encodePlain(text[start:i], &ids); err != nil {
				return nil, err
			}
			ids = append(ids, a.id)
			i += len(a.content)
			start = i
			continue
		}
		_, w := utf8.DecodeRuneInString(text[i:])
		i += w
	}
	if err := t.encodePlain(text[start:], &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func (t *Tokenizer) addedAt(s string) (addedToken, bool) {
	for _, a := range t.added {
		if strings.HasPrefix(s, a.content) {
			return a, true
		}
	}
	return addedToken{}, false
}

func (t *Tokenizer) encodePlain(s string, ids *[]int64) error {
	for _, word := range preTokenize(s) {
		var sym []string
		for _, b := range []byte(word) {
			sym = append(sym, string(t.byteCh[b]))
		}
		for _, piece := range t.bpe(sym) {
			id, ok := t.vocab[piece]
			if !ok {
				return fmt.Errorf("tokenizer: %q is not in the vocabulary", piece)
			}
			*ids = append(*ids, id)
		}
	}
	return nil
}

// bpe merges the adjacent pair of lowest rank, leftmost first, until no
// pair has a rank.
func (t *Tokenizer) bpe(sym []string) []string {
	for len(sym) > 1 {
		best, at := -1, -1
		for i := 0; i+1 < len(sym); i++ {
			if r, ok := t.ranks[[2]string{sym[i], sym[i+1]}]; ok && (best < 0 || r < best) {
				best, at = r, i
			}
		}
		if at < 0 {
			break
		}
		sym[at] += sym[at+1]
		sym = append(sym[:at+1], sym[at+2:]...)
	}
	return sym
}

// preTokenize splits s by the ByteLevel pattern
//
//	's|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+(?!\S)|\s+
//
// written out by hand: Go's regexp has no lookahead.
func preTokenize(s string) []string { return byteLevel([]rune(s)) }

var contractions = []string{"s", "t", "re", "ve", "m", "ll", "d"}

func byteLevel(r []rune) []string {
	var out []string
	run := func(i int, f func(rune) bool) int {
		for i < len(r) && f(r[i]) {
			i++
		}
		return i
	}
	for i := 0; i < len(r); {
		j := matchAt(r, i, run)
		out = append(out, string(r[i:j]))
		i = j
	}
	return out
}

func matchAt(r []rune, i int, run func(int, func(rune) bool) int) int {
	if r[i] == '\'' {
		for _, c := range contractions {
			if hasRunes(r[i+1:], c) {
				return i + 1 + len(c)
			}
		}
	}
	for _, f := range []func(rune) bool{isLetter, isNumber, isOther} {
		k := i
		if r[k] == ' ' && k+1 < len(r) {
			k++
		}
		if f(r[k]) {
			return run(k, f)
		}
	}
	// \s+(?!\S), then \s+.
	j := run(i, isSpace)
	if j == len(r) || j-i == 1 {
		return j
	}
	return j - 1
}

func hasRunes(r []rune, s string) bool {
	k := 0
	for _, c := range s {
		if k >= len(r) || r[k] != c {
			return false
		}
		k++
	}
	return true
}

// Task is what lerobot's processors make of a task: a newline added when
// the text has none, the ids truncated on the left to Width (the
// backbone's truncation_side), then padded on the right with the pad
// token; the mask is 1 for each real token.
func (t *Tokenizer) Task(text string) (ids, mask [Width]int64, err error) {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	all, err := t.Encode(text)
	if err != nil {
		return ids, mask, err
	}
	if len(all) > Width {
		all = all[len(all)-Width:]
	}
	for i := range ids {
		if i < len(all) {
			ids[i], mask[i] = all[i], 1
		} else {
			ids[i] = t.pad
		}
	}
	return ids, mask, nil
}
