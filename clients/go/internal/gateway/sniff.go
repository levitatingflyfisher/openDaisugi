package gateway

import (
	"strings"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is the SSE usage sniffers: gateway_asgi._UsageSniffer for the
// Anthropic wire and gateway_openai.OpenAIUsageSniffer. Each reads the
// stream line by line as it passes, never holding it back.

// Usage is the usage dict a sniffer builds: keys in first-set order.
type Usage struct{ obj *pyjson.Object }

// Value is the usage as the meter reads it.
func (u *Usage) Value() any {
	if u.obj == nil {
		return pyjson.NewObject()
	}
	return u.obj
}

func (u *Usage) update(k string, v any) {
	if u.obj == nil {
		u.obj = pyjson.NewObject()
	}
	u.obj.Set(k, v)
}

// Sniffer is either wire's sniffer.
type Sniffer struct {
	openai bool
	buf    string
	Usage  Usage
	Text   strings.Builder
	// Model is message_start's message.model, nil when none was a str.
	Model *string
	// Raised is set when json.loads raised something the sniffer does
	// not catch (a RecursionError, an integer past the digit limit): the
	// oracle's stream then ends there with an error.
	Raised bool
}

// NewSniffer returns the sniffer for a wire.
func NewSniffer(openai bool) *Sniffer { return &Sniffer{openai: openai} }

// Feed reads one chunk: decoded on its own with replacement characters,
// as the oracle decodes each chunk.
func (s *Sniffer) Feed(chunk []byte) {
	if s.Raised {
		return
	}
	s.buf += pystr.DecodeReplace(chunk)
	for {
		i := strings.IndexByte(s.buf, '\n')
		if i < 0 {
			return
		}
		line := pystr.Strip(s.buf[:i])
		s.buf = s.buf[i+1:]
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := pystr.Strip(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		obj, derr := pyjson.LoadsPy(payload, maxJSONDepth)
		if derr != nil {
			if derr.NotJSON || derr.TooDeep {
				s.Raised = true
				return
			}
			continue
		}
		if s.openai {
			s.openaiEvent(obj)
		} else {
			s.anthropicEvent(obj)
		}
	}
}

func (s *Sniffer) anthropicEvent(obj any) {
	typ, err := get(obj, "type", nil)
	if err != nil {
		return
	}
	var u any
	switch {
	case eqStr(typ, "message_start"):
		message := getOr(obj.(*pyjson.Object), "message", pyjson.NewObject())
		mu, err := get(message, "usage", nil)
		if err != nil {
			return
		}
		u = mu
		if m, ok := getOr(message.(*pyjson.Object), "model", nil).(string); ok {
			s.Model = &m
		}
	case eqStr(typ, "message_delta"):
		u = getOr(obj.(*pyjson.Object), "usage", nil)
	case eqStr(typ, "content_block_delta"):
		delta, ok := getOr(obj.(*pyjson.Object), "delta", nil).(*pyjson.Object)
		if ok && eqStr(getOr(delta, "type", nil), "text_delta") {
			if t, ok := getOr(delta, "text", nil).(string); ok {
				s.Text.WriteString(t)
			}
		}
	}
	if o, ok := u.(*pyjson.Object); ok {
		for _, k := range o.Keys() {
			if isPyInt(o.Value(k)) {
				s.Usage.update(k, o.Value(k))
			}
		}
	}
}

// isPyInt is isinstance(v, int): a bool is an int.
func isPyInt(v any) bool {
	switch v.(type) {
	case pyjson.Int, bool:
		return true
	}
	return false
}

func (s *Sniffer) openaiEvent(obj any) {
	choices, err := get(obj, "choices", nil)
	if err != nil {
		return
	}
	if !pyjson.Truthy(choices) {
		choices = []any{}
	}
	items, err := iterate(choices)
	if err != nil {
		return
	}
	for _, c := range items {
		o, ok := c.(*pyjson.Object)
		if !ok {
			return
		}
		if delta, ok := getOr(o, "delta", nil).(*pyjson.Object); ok {
			if t, ok := getOr(delta, "content", nil).(string); ok {
				// A raise at a later choice keeps this text: the
				// oracle appends as it goes.
				s.Text.WriteString(t)
			}
		}
	}
	n := NormalizeOpenAIUsage(getOr(obj.(*pyjson.Object), "usage", nil))
	if n.Len() > 0 {
		for _, k := range n.Keys() {
			s.Usage.update(k, n.Value(k))
		}
	}
}

// NormalizeOpenAIUsage is normalize_openai_usage: OpenAI usage on the
// meter's Anthropic-named buckets.
func NormalizeOpenAIUsage(usage any) *pyjson.Object {
	out := pyjson.NewObject()
	u, ok := usage.(*pyjson.Object)
	if !ok {
		return out
	}
	prompt := getOr(u, "prompt_tokens", nil)
	completion := getOr(u, "completion_tokens", nil)
	var cached any = pyjson.Int{Text: "0"}
	if d, ok := getOr(u, "prompt_tokens_details", nil).(*pyjson.Object); ok {
		cached = getOr(d, "cached_tokens", pyjson.Int{Text: "0"})
	}
	if !isPyInt(cached) {
		cached = pyjson.Int{Text: "0"}
	}
	if isPyInt(prompt) {
		p, _ := pyInt(prompt)
		c, _ := pyInt(cached)
		d := p.Sub(p, c)
		if d.Sign() < 0 {
			d.SetInt64(0)
		}
		out.Set("input_tokens", pyjson.Int{Text: d.String()})
		out.Set("cache_read_input_tokens", cached)
		out.Set("cache_creation_input_tokens", pyjson.Int{Text: "0"})
	}
	if isPyInt(completion) {
		out.Set("output_tokens", completion)
	}
	return out
}
