package sprig

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const fakeKey = "sk-FAKE-redirect-test"

// A redirect is an error on both wires: sprig follows none, so the
// second host never sees the key or the request.
func TestNoWireFollowsARedirect(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		var hits atomic.Int32
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Write([]byte(`{"choices":[{"message":{"content":"took it"}}]}`))
		}))
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			to := strings.Replace(second.URL, "127.0.0.1", "localhost", 1) + r.URL.Path
			w.Header().Set("Location", to)
			w.WriteHeader(status)
			w.Write([]byte("moved"))
		}))
		models := map[string]Model{
			"anthropic": &APIModel{APIKey: fakeKey, Model: "m", BaseURL: first.URL},
			"openai":    &OpenAIModel{APIKey: fakeKey, Model: "m", BaseURL: first.URL + "/v1"},
		}
		for wire, m := range models {
			_, err := m.Next([]Message{{Role: "user", Text: "x"}})
			if err == nil {
				t.Fatalf("%s %d: followed the redirect", wire, status)
			}
			if !strings.HasPrefix(err.Error(), wire+" api "+strconv.Itoa(status)+":") || strings.Contains(err.Error(), fakeKey) {
				t.Fatalf("%s %d: %v", wire, status, err)
			}
		}
		if hits.Load() != 0 {
			t.Fatalf("%d: the second host got %d requests", status, hits.Load())
		}
		first.Close()
		second.Close()
	}
}

// A response past 32 MiB fails the call with one plain line.
func TestAModelResponseIsCapped(t *testing.T) {
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat("x", 1<<20))
		for i := 0; i < 33; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer big.Close()
	models := map[string]Model{
		"anthropic": &APIModel{Model: "m", BaseURL: big.URL},
		"openai":    &OpenAIModel{Model: "m", BaseURL: big.URL + "/v1"},
	}
	for wire, m := range models {
		_, err := m.Next([]Message{{Role: "user", Text: "x"}})
		if err == nil || err.Error() != wire+" api: the response is larger than 32 MiB" {
			t.Fatalf("%s: %v", wire, err)
		}
	}
}
