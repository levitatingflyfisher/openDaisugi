package potion

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Tokenizer is the part of a tokenizers tokenizer.json that model2vec's
// encode_batch_fast(texts, add_special_tokens=False) runs for a potion
// model: added tokens split out of the raw text, BertNormalizer,
// BertPreTokenizer and WordPiece. Any other component is refused at load,
// never guessed at.
type Tokenizer struct {
	vocab     map[string]int
	unk       string
	unkID     int
	prefix    string
	maxChars  int
	added     []addedToken // longest first
	allTokens map[string]int
}

type addedToken struct {
	content string
	id      int
}

type rawTokenizer struct {
	Truncation    json.RawMessage `json:"truncation"`
	Padding       json.RawMessage `json:"padding"`
	AddedTokens   []rawAdded      `json:"added_tokens"`
	Normalizer    json.RawMessage `json:"normalizer"`
	PreTokenizer  json.RawMessage `json:"pre_tokenizer"`
	Model         json.RawMessage `json:"model"`
	PostProcessor json.RawMessage `json:"post_processor"`
	Decoder       json.RawMessage `json:"decoder"`
}

type rawAdded struct {
	ID         int    `json:"id"`
	Content    string `json:"content"`
	SingleWord bool   `json:"single_word"`
	LStrip     bool   `json:"lstrip"`
	RStrip     bool   `json:"rstrip"`
	Normalized bool   `json:"normalized"`
	Special    bool   `json:"special"`
}

func isNull(m json.RawMessage) bool { return len(m) == 0 || string(m) == "null" }

// ParseTokenizer reads tokenizer.json.
func ParseTokenizer(data []byte) (*Tokenizer, error) {
	var raw rawTokenizer
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("tokenizer.json: %v", err)
	}
	if !isNull(raw.Truncation) || !isNull(raw.Padding) {
		return nil, fmt.Errorf("tokenizer.json sets truncation or padding, which this binary does not model")
	}
	var norm struct {
		Type               string `json:"type"`
		CleanText          bool   `json:"clean_text"`
		HandleChineseChars bool   `json:"handle_chinese_chars"`
		StripAccents       *bool  `json:"strip_accents"`
		Lowercase          bool   `json:"lowercase"`
	}
	if err := json.Unmarshal(raw.Normalizer, &norm); err != nil || norm.Type != "BertNormalizer" ||
		!norm.CleanText || !norm.HandleChineseChars || !norm.Lowercase ||
		(norm.StripAccents != nil && !*norm.StripAccents) {
		return nil, fmt.Errorf("tokenizer.json has a normalizer this binary does not model (only BertNormalizer with clean_text, handle_chinese_chars and lowercase)")
	}
	var pre struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw.PreTokenizer, &pre); err != nil || pre.Type != "BertPreTokenizer" {
		return nil, fmt.Errorf("tokenizer.json has a pre_tokenizer this binary does not model (only BertPreTokenizer)")
	}
	var model struct {
		Type     string         `json:"type"`
		Unk      *string        `json:"unk_token"`
		Prefix   string         `json:"continuing_subword_prefix"`
		MaxChars int            `json:"max_input_chars_per_word"`
		Vocab    map[string]int `json:"vocab"`
	}
	if err := json.Unmarshal(raw.Model, &model); err != nil || model.Type != "WordPiece" || model.Unk == nil {
		return nil, fmt.Errorf("tokenizer.json has a model this binary does not model (only WordPiece with an unk_token)")
	}
	unkID, ok := model.Vocab[*model.Unk]
	if !ok {
		return nil, fmt.Errorf("tokenizer.json: the unk_token %q is not in the vocabulary", *model.Unk)
	}
	t := &Tokenizer{
		vocab: model.Vocab, unk: *model.Unk, unkID: unkID, prefix: model.Prefix, maxChars: model.MaxChars,
		allTokens: map[string]int{},
	}
	for k, v := range model.Vocab {
		t.allTokens[k] = v
	}
	for _, a := range raw.AddedTokens {
		if a.SingleWord || a.LStrip || a.RStrip || a.Normalized || a.Content == "" {
			return nil, fmt.Errorf("tokenizer.json has an added token (%q) with options this binary does not model", a.Content)
		}
		t.added = append(t.added, addedToken{a.Content, a.ID})
		t.allTokens[a.Content] = a.ID
	}
	sort.SliceStable(t.added, func(i, j int) bool { return len(t.added[i].content) > len(t.added[j].content) })
	return t, nil
}

// UnkID is the id of the unknown token, which model2vec drops.
func (t *Tokenizer) UnkID() int { return t.unkID }

// Vocab is get_vocab(): the model's vocabulary with the added tokens.
func (t *Tokenizer) Vocab() map[string]int { return t.allTokens }

// Encode is encode(text, add_special_tokens=False).ids.
func (t *Tokenizer) Encode(text string) []int {
	var ids []int
	for len(text) > 0 {
		at, tok := t.findAdded(text)
		if at < 0 {
			ids = t.encodeSegment(text, ids)
			break
		}
		ids = t.encodeSegment(text[:at], ids)
		ids = append(ids, tok.id)
		text = text[at+len(tok.content):]
	}
	return ids
}

// findAdded is the leftmost added token in text, the longest one at that
// place.
func (t *Tokenizer) findAdded(text string) (int, addedToken) {
	best, bestTok := -1, addedToken{}
	for _, a := range t.added {
		i := strings.Index(text, a.content)
		if i >= 0 && (best < 0 || i < best) {
			best, bestTok = i, a
		}
	}
	return best, bestTok
}

func (t *Tokenizer) encodeSegment(seg string, ids []int) []int {
	if seg == "" {
		return ids
	}
	for _, word := range preTokenize(normalize(seg)) {
		ids = t.wordPiece(word, ids)
	}
	return ids
}

// wordPiece is WordPiece.tokenize for one word: greedy longest match,
// the prefix on every piece after the first, the whole word unknown when
// one piece has no match or the word is too long.
func (t *Tokenizer) wordPiece(word string, ids []int) []int {
	if utf8.RuneCountInString(word) > t.maxChars {
		return append(ids, t.unkID)
	}
	var sub []int
	start := 0
	for start < len(word) {
		end := len(word)
		found := -1
		for start < end {
			s := word[start:end]
			if start > 0 {
				s = t.prefix + s
			}
			if id, ok := t.vocab[s]; ok {
				found = id
				break
			}
			_, size := utf8.DecodeLastRuneInString(word[start:end])
			end -= size
		}
		if found < 0 {
			return append(ids, t.unkID)
		}
		sub = append(sub, found)
		start = end
	}
	return append(ids, sub...)
}

type rng struct{ lo, hi rune }

type mapped struct {
	r rune
	s string
}

func lookupMapped(r rune) (string, bool) {
	i := sort.Search(len(normMapped), func(i int) bool { return normMapped[i].r >= r })
	if i < len(normMapped) && normMapped[i].r == r {
		return normMapped[i].s, true
	}
	return "", false
}

func inTable(tab []rng, r rune) bool {
	i := sort.Search(len(tab), func(i int) bool { return tab[i].hi >= r })
	return i < len(tab) && tab[i].lo <= r
}

// normalize is BertNormalizer, one code point at a time.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case inTable(normRemoved, r):
		case inTable(normSpace, r):
			b.WriteByte(' ')
		case inTable(normCJK, r):
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
		case r >= 0xAC00 && r <= 0xD7A3:
			n := r - 0xAC00
			b.WriteRune(0x1100 + n/588)
			b.WriteRune(0x1161 + (n%588)/28)
			if tail := 0x11A7 + n%28; tail != 0x11A7 {
				b.WriteRune(tail)
			}
		default:
			if m, ok := lookupMapped(r); ok {
				b.WriteString(m)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// preTokenize is BertPreTokenizer: split on whitespace (dropped), then
// every punctuation mark is a word of its own.
func preTokenize(s string) []string {
	var out []string
	start := -1
	for i, r := range s {
		switch {
		case inTable(preSpace, r):
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
		case inTable(prePunct, r):
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			out = append(out, string(r))
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}
