package llm

import "testing"

func TestBodyBytes(t *testing.T) {
	got := string(body("claude-x", "S é", []message{{"user", "U \x01\"\\"}}, 4096))
	want := `{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"U` + " " +
		`\u0001\"\\"}]}],"system":[{"type":"text","text":"S é"}],"max_tokens":4096}`
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestAPIError(t *testing.T) {
	for status, want := range map[int]string{
		500: "litellm.InternalServerError: AnthropicException - b. Handle with `litellm.InternalServerError`.",
		401: "litellm.AuthenticationError: AnthropicException - b",
		502: "litellm.BadGatewayError: AnthropicException BadGatewayError - b",
		503: "litellm.ServiceUnavailableError: AnthropicException - b. Handle with `litellm.ServiceUnavailableError`.",
		418: "litellm.BadRequestError: AnthropicException - b",
		408: "litellm.Timeout: AnthropicException - b",
	} {
		if got := apiError(status, "b"); got != want {
			t.Errorf("%d: %s", status, got)
		}
	}
}

func TestResultText(t *testing.T) {
	if s, err := resultText(`{"type":"result","is_error":false,"result":"hi"}`); err != nil || s != "hi" {
		t.Fatal(s, err)
	}
	if _, err := resultText(`{"type":"result","is_error":true,"result":"no"}`); err == nil || err.Error() != "claude -p reported is_error: 'no'" {
		t.Fatal(err)
	}
	if _, err := resultText(`{"a":1}`); err == nil || err.Error() != `claude -p stdout was not its JSON envelope: '{"a":1}'` {
		t.Fatal(err)
	}
}

func TestMaxTokens(t *testing.T) {
	c := New(Env{Getenv: func(string) (string, bool) { return "", false }})
	if c.maxTokens("claude-sonnet-4-20250514") != 4096 || c.maxTokens("claude-haiku-4-5") != 64000 {
		t.Fatal(c.maxTokens("claude-sonnet-4-20250514"), c.maxTokens("claude-haiku-4-5"))
	}
}
