package sprig

import "testing"

// marks counts the cache marks in a request's system and messages.
func marks(system []apiBlock, msgs []apiMessage) int {
	n := 0
	for _, b := range system {
		if b.CacheControl != nil {
			n++
		}
	}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				n++
			}
		}
	}
	return n
}

func TestTheEndMarkMovesWithEachToolRoundAndThereAreNeverMoreThanTwo(t *testing.T) {
	history := []Message{{Role: "user", Text: "go"}}
	sys := []apiBlock{{Type: "text", Text: "S", CacheControl: ephemeral}}
	for round := 0; round < 3; round++ {
		msgs := buildMessages(history, true)
		if n := marks(sys, msgs); n != 2 {
			t.Fatalf("round %d: %d marks", round, n)
		}
		last := msgs[len(msgs)-1]
		if last.Content[len(last.Content)-1].CacheControl == nil {
			t.Fatalf("round %d: the last block has no mark", round)
		}
		history = append(history,
			Message{Role: "assistant", Calls: []ToolCall{{Name: "read", Input: map[string]any{"path": "a"}}}},
			Message{Role: "tool", Text: "A"})
	}
}

func TestAnEmptyLastBlockGetsNoMark(t *testing.T) {
	msgs := buildMessages([]Message{{Role: "user", Text: "before"}, {Role: "user", Text: ""}}, true)
	c := msgs[0].Content
	if len(c) != 2 || c[1].Text != "" || c[1].CacheControl != nil || c[0].CacheControl == nil {
		t.Fatalf("content = %+v", c)
	}
	if n := marks(nil, msgs); n != 1 {
		t.Fatalf("%d marks", n)
	}
}

func TestNoEndMarkMeansNoMarksAtAll(t *testing.T) {
	msgs := buildMessages([]Message{{Role: "user", Text: "go"}}, false)
	if n := marks(nil, msgs); n != 0 {
		t.Fatalf("%d marks", n)
	}
}

// requestMarks sends one request and counts the marks in its body.
func requestMarks(t *testing.T, m *APIModel) (system, end int) {
	t.Helper()
	var got map[string]any
	m.httpDo = capture(t, &got, `{"content":[{"type":"text","text":"ok"}]}`)
	if _, err := m.Next([]Message{{Role: "user", Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if blocks, ok := got["system"].([]any); ok {
		for _, b := range blocks {
			if b.(map[string]any)["cache_control"] != nil {
				system++
			}
		}
	}
	for _, msg := range got["messages"].([]any) {
		for _, b := range msg.(map[string]any)["content"].([]any) {
			if b.(map[string]any)["cache_control"] != nil {
				end++
			}
		}
	}
	return system, end
}

func TestASystemFileWithToolsMarksTheSystemBlockAndTheEnd(t *testing.T) {
	m := &APIModel{Model: "m", NoTools: true, System: "SYS FILE", SystemSet: true, Extra: []MCPToolDef{{Name: "mcp__s__t"}}}
	if s, e := requestMarks(t, m); s != 1 || e != 1 {
		t.Fatalf("marks: system %d, end %d", s, e)
	}
	m = &APIModel{Model: "m", System: "SYS FILE", SystemSet: true}
	if s, e := requestMarks(t, m); s != 1 || e != 1 {
		t.Fatalf("marks with the wall's tools: system %d, end %d", s, e)
	}
}

func TestAOneShotCallMarksOnlyTheSystemBlock(t *testing.T) {
	m := &APIModel{Model: "m", NoTools: true, System: "SYS FILE", SystemSet: true}
	if s, e := requestMarks(t, m); s != 1 || e != 0 {
		t.Fatalf("marks: system %d, end %d", s, e)
	}
}

func TestNoSystemFileMeansNoMarks(t *testing.T) {
	m := &APIModel{Model: "m"}
	if s, e := requestMarks(t, m); s != 0 || e != 0 {
		t.Fatalf("marks: system %d, end %d", s, e)
	}
}

func TestAnEmptySystemFileIsNeverAMarkedBlock(t *testing.T) {
	m := &APIModel{Model: "m", NoTools: true, SystemSet: true, Extra: []MCPToolDef{{Name: "mcp__s__t"}}}
	if s, e := requestMarks(t, m); s != 0 || e != 1 {
		t.Fatalf("marks: system %d, end %d", s, e)
	}
}
