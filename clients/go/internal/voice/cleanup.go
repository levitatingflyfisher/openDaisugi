package voice

import (
	"math/big"
	"os"
	"path/filepath"
	"time"

	"daisugi-verify/internal/gateway"
	"daisugi-verify/internal/llm"
	"daisugi-verify/internal/pystr"
)

// CleanupPrompt is cleanup.CLEANUP_PROMPT_V1.
const CleanupPrompt = "Fix punctuation and obvious transcription errors. Keep the meaning and " +
	"the words. Return only the corrected text."

const (
	noTextReason       = "The cleanup model returned no text. Check the model, then try again."
	unreachableReason  = "The cleanup model failed to respond. Check that it is running, then try again."
	unwritableReason   = "The transcript could not be journaled. Check the data directory, then try again."
	cleanupRouteReason = "voice cleanup runs on a fixed local model. It is never routed."
)

// CleanupConfig is the part of Config clean_transcript reads.
type CleanupConfig struct {
	On      bool
	Model   *string
	BaseURL *string
	DataDir string
}

// CleanupResult is cleanup.CleanupResult.
type CleanupResult struct {
	Text    string
	Cleaned bool
	Reason  *string
}

// Transport is one blocking completion call: the corrected text and the
// input and output token counts.
type Transport interface {
	Complete(model, system, user string) (string, *big.Int, *big.Int, error)
}

// HTTPTransport is cleanup.HTTPCleanupTransport over the llm client.
type HTTPTransport struct {
	Client  *llm.Client
	BaseURL *string
	Timeout float64
}

// Complete prefixes a bare model name with openai/ when a base URL is
// set, as HTTPCleanupTransport does.
func (t HTTPTransport) Complete(model, system, user string) (string, *big.Int, *big.Int, error) {
	call := model
	base := ""
	if t.BaseURL != nil {
		base = *t.BaseURL
		if !containsSlash(call) {
			call = "openai/" + call
		}
	}
	reply, err := t.Client.CompleteAt(call, base, []llm.Message{{Role: "system", Content: system}, {Role: "user", Content: user}},
		llm.BodyOpts{MaxTokens: -1}, t.Timeout)
	if err != nil {
		return "", nil, nil, err
	}
	in, out := big.NewInt(0), big.NewInt(0)
	if reply.InputTokens != nil {
		in.SetInt64(*reply.InputTokens)
	}
	if reply.OutputTokens != nil {
		out.SetInt64(*reply.OutputTokens)
	}
	return reply.Text, in, out, nil
}

func containsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return true
		}
	}
	return false
}

// CleanTranscript is cleanup.clean_transcript. A journal error that is
// not a file error is returned as err, which the server answers 500, as
// the oracle's would.
func CleanTranscript(text string, cfg CleanupConfig, tr Transport, now time.Time) (CleanupResult, error) {
	if !cfg.On || cfg.Model == nil || *cfg.Model == "" {
		return CleanupResult{Text: text}, nil
	}
	model := *cfg.Model
	corrected, in, out, err := tr.Complete(model, CleanupPrompt, text)
	if err != nil {
		return CleanupResult{Text: text, Reason: strp(unreachableReason)}, nil
	}
	cleaned := pystr.Strip(corrected)
	if cleaned == "" {
		return CleanupResult{Text: text, Reason: strp(noTextReason)}, nil
	}
	line, err := gateway.LocalTurnLine(model, text, cleanupRouteReason, in, out, now)
	if err != nil {
		return CleanupResult{}, err
	}
	if err := appendLine(filepath.Join(cfg.DataDir, "gateway", "turns.jsonl"), line); err != nil {
		return CleanupResult{Text: cleaned, Reason: strp(unwritableReason)}, nil
	}
	return CleanupResult{Text: cleaned, Cleaned: true}, nil
}

// appendLine is GatewayJournal.append: the parent made, one line added.
func appendLine(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
