package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// client talks HTTP to one running `opencode serve`. Every endpoint, field
// and status code it uses is recorded in PINS.md.
type client struct {
	baseURL  string
	username string
	password string
	// http carries every short request. Its timeout bounds the whole
	// request, body included.
	http *http.Client
	// stream carries GET /event only. The event stream stays open for as
	// long as the server lives, so it has no whole-request timeout. The
	// caller's context ends it.
	stream *http.Client
}

func newClient(baseURL, username, password string) *client {
	return &client{
		baseURL:  baseURL,
		username: username,
		password: password,
		http:     &http.Client{Timeout: 10 * time.Second},
		stream:   &http.Client{},
	}
}

func (c *client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("opencode: encode request: %w", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, r)
	if err != nil {
		return nil, fmt.Errorf("opencode: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.username, c.password)
	return req, nil
}

func (c *client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	req, err := c.request(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	return c.http.Do(req)
}

// call sends one request and refuses any status outside 2xx. It reads and
// closes the body, and returns it.
func (c *client) call(ctx context.Context, what, method, path string, body any) ([]byte, error) {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return nil, fmt.Errorf("opencode: %s: %w", what, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("opencode: %s: %s: %s", what, resp.Status, bytes.TrimSpace(b))
	}
	return b, nil
}

// seg escapes one path segment, so an id can never name another path.
func seg(s string) string { return url.PathEscape(s) }

// waitReady polls GET /global/health until it answers 200 or ctx ends.
func (c *client) waitReady(ctx context.Context) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		resp, err := c.do(ctx, http.MethodGet, "/global/health", nil)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("opencode: the server did not answer /global/health: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// createSession calls POST /session and returns the new session id.
func (c *client) createSession(ctx context.Context, title string) (string, error) {
	b, err := c.call(ctx, "create a session", http.MethodPost, "/session", map[string]string{"title": title})
	if err != nil {
		return "", err
	}
	var info struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &info); err != nil {
		return "", fmt.Errorf("opencode: read the new session: %w", err)
	}
	if info.ID == "" {
		return "", fmt.Errorf("opencode: the new session has no id")
	}
	return info.ID, nil
}

// sessionExists calls GET /session/{id}. Any answer but 200 is false.
func (c *client) sessionExists(ctx context.Context, id string) bool {
	resp, err := c.do(ctx, http.MethodGet, "/session/"+seg(id), nil)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// promptAsync calls POST /session/{id}/prompt_async with one text part. The
// server answers at once. The turn's progress arrives on the event stream.
func (c *client) promptAsync(ctx context.Context, sessionID, text string) error {
	body := map[string]any{"parts": []map[string]string{{"type": "text", "text": text}}}
	_, err := c.call(ctx, "send a prompt", http.MethodPost, "/session/"+seg(sessionID)+"/prompt_async", body)
	return err
}

// replyPermission answers OpenCode's own permission prompt: once, always or
// reject. message goes back to the model with a reject.
func (c *client) replyPermission(ctx context.Context, id, reply, message string) error {
	body := map[string]string{"reply": reply}
	if message != "" {
		body["message"] = message
	}
	_, err := c.call(ctx, "answer a permission prompt", http.MethodPost, "/permission/"+seg(id)+"/reply", body)
	return err
}

// replyQuestion answers a pending question tool call. answers holds one
// list of labels per question, in order.
func (c *client) replyQuestion(ctx context.Context, id string, answers [][]string) error {
	_, err := c.call(ctx, "answer a question", http.MethodPost, "/question/"+seg(id)+"/reply",
		map[string]any{"answers": answers})
	return err
}

// rejectQuestion answers a pending question tool call with no answer.
func (c *client) rejectQuestion(ctx context.Context, id string) error {
	_, err := c.call(ctx, "reject a question", http.MethodPost, "/question/"+seg(id)+"/reject", nil)
	return err
}

// openEvents opens GET /event. The caller reads the body until it ends and
// closes it.
func (c *client) openEvents(ctx context.Context) (*http.Response, error) {
	req, err := c.request(ctx, http.MethodGet, "/event", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode: open the event stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("opencode: open the event stream: %s", resp.Status)
	}
	return resp, nil
}
