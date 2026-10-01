package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// llama-server puts the served window in data[0].meta.n_ctx and keeps the
// training length beside it. The training length is not what is served.
const llamaModelsBody = `{"object":"list","data":[{"id":"gemma","object":"model","owned_by":"llamacpp",` +
	`"meta":{"vocab_type":2,"n_vocab":262144,"n_ctx":16384,"n_ctx_train":262144,"n_embd":3840}}]}`

func probeServer(t *testing.T, models, props string, propsHits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(models))
		case "/props":
			propsHits.Add(1)
			if props == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(props))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRemoteContextFromModelsMeta(t *testing.T) {
	var props atomic.Int32
	srv := probeServer(t, llamaModelsBody, "", &props)
	code, out, errs := setupEnv(t, nil, "--remote", strings.TrimPrefix(srv.URL, "http://"), "--kind", "openai")
	if code != 0 || !strings.Contains(out, "16384") || strings.Contains(out, "262144") || props.Load() != 0 {
		t.Fatalf("code %d, props hits %d\n%s\n%s", code, props.Load(), out, errs)
	}
}

func TestRemoteContextFallsBackToProps(t *testing.T) {
	var props atomic.Int32
	srv := probeServer(t, `{"data":[{"id":"m","meta":{"n_ctx_train":262144}}]}`,
		`{"default_generation_settings":{"n_ctx":8192},"total_slots":1}`, &props)
	code, out, errs := setupEnv(t, nil, "--remote", strings.TrimPrefix(srv.URL, "http://"), "--kind", "openai")
	if code != 0 || !strings.Contains(out, "8192") || strings.Contains(out, "262144") || props.Load() != 1 {
		t.Fatalf("code %d, props hits %d\n%s\n%s", code, props.Load(), out, errs)
	}
}

// --endpoint reports the window the server serves, when it says.
func TestEndpointReportsServedContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(llamaModelsBody))
		case "/v1/chat/completions":
			chatReply(w, map[string]any{"role": "assistant", "content": validEnvelopeReply}, "stop")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	code, out, errs := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m")
	if code != 0 || !strings.Contains(out, "PASSED") || !strings.Contains(out, "Context window: 16384 tokens") ||
		strings.Contains(out, "262144") {
		t.Fatalf("code %d\n%s\n%s", code, out, errs)
	}
	_, js, _ := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "m", "--json")
	if !strings.Contains(js, `"context_window": 16384`) {
		t.Fatalf("%s", js)
	}
}

// With several models served, --endpoint reports the window of --model, and
// of the first entry when --model is not among them.
func TestEndpointContextIsOfTheNamedModel(t *testing.T) {
	models := `{"data":[{"id":"small","meta":{"n_ctx":4096}},{"id":"big","meta":{"n_ctx":32768}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(models))
		case "/v1/chat/completions":
			chatReply(w, map[string]any{"role": "assistant", "content": validEnvelopeReply}, "stop")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	_, out, _ := setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "big")
	if !strings.Contains(out, "Context window: 32768 tokens") {
		t.Fatalf("%s", out)
	}
	_, out, _ = setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "openai/big")
	if !strings.Contains(out, "Context window: 32768 tokens") {
		t.Fatalf("a provider prefix hid the model\n%s", out)
	}
	_, out, _ = setupEnv(t, nil, "--endpoint", srv.URL+"/v1", "--model", "other")
	if !strings.Contains(out, "Context window: 4096 tokens") {
		t.Fatalf("%s", out)
	}
}

func TestRemoteContextNeverTheTrainingLength(t *testing.T) {
	var props atomic.Int32
	srv := probeServer(t, `{"data":[{"id":"m","n_ctx_train":4096}]}`, "", &props)
	_, out, _ := setupEnv(t, nil, "--remote", strings.TrimPrefix(srv.URL, "http://"), "--kind", "openai")
	if strings.Contains(out, "4096") {
		t.Fatalf("the training length was read as the served window\n%s", out)
	}
}
