package gateway

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"daisugi-verify/internal/pyjson"
	"daisugi-verify/internal/pystr"
)

// This file is opendaisugi.route_model: the smart end of the router slot. A
// model that picks among the allowed names, and a model that grades a finished
// turn. Both go through the gateway's own upstream path. The prompts are fixed
// text; the bytes match the Python gateway's.

const (
	textLimit  = 2000
	askTimeout = 15 * time.Second
)

func helperBody(model, prompt string, maxTokens int) []byte {
	msg := pyjson.NewObject().Set("role", "user").Set("content", prompt)
	body := pyjson.NewObject().Set("model", model).Set("max_tokens", pyjson.Int{Text: strconv.Itoa(maxTokens)}).
		Set("messages", []any{msg})
	return []byte(pyjson.Dumps(body, true))
}

// PickerBody is picker_body.
func PickerBody(routerModel, task string, candidates []string) []byte {
	prompt := "Pick the model for this task. Allowed models: " + strings.Join(candidates, ", ") + ".\n" +
		"Reply with one name from the list and nothing else.\n\n" +
		"Task:\n" + pystr.Slice(task, 0, textLimit)
	return helperBody(routerModel, prompt, 20)
}

// ParsePick is parse_pick: the allowed name the model answered, or "".
func ParsePick(text string, ok bool, candidates []string) string {
	if !ok {
		return ""
	}
	answer := strings.Trim(pystr.Strip(text), "`'\".")
	for _, c := range candidates {
		if c == answer {
			return answer
		}
	}
	return ""
}

// GraderBody is grader_body.
func GraderBody(graderModel, task, answer string) []byte {
	prompt := "Task:\n" + pystr.Slice(task, 0, textLimit) + "\n\nAnswer:\n" + pystr.Slice(answer, 0, textLimit) +
		"\n\nDid the answer do the task? Reply with one word: good or bad."
	return helperBody(graderModel, prompt, 5)
}

// ParseGrade is parse_grade: good or bad, or "".
func ParseGrade(text string, ok bool) string {
	if !ok {
		return ""
	}
	word := strings.ToLower(strings.Trim(pystr.Strip(text), "`'\"."))
	if word == "good" || word == "bad" {
		return word
	}
	return ""
}

// ReplyText is reply_text: the text of a Messages reply.
func ReplyText(v any) (string, bool) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return "", false
	}
	content, ok := getOr(o, "content", nil).([]any)
	if !ok {
		return "", false
	}
	var sb strings.Builder
	found := false
	for _, b := range content {
		bo, ok := b.(*pyjson.Object)
		if !ok || !eqStr(getOr(bo, "type", nil), "text") {
			continue
		}
		if t, ok := getOr(bo, "text", nil).(string); ok {
			sb.WriteString(t)
			found = true
		}
	}
	return sb.String(), found
}

// Off is off(): "" when unset or "off".
func Off(v *string) string {
	if v == nil {
		return ""
	}
	s := pystr.Strip(*v)
	if s == "" || strings.ToLower(s) == "off" {
		return ""
	}
	return s
}

// Choice is the router model's answer, as prepare takes it: the picked
// name, or the reason there is none.
type Choice struct {
	Name string
	Why  string
}

// PickRequest is Gateway.pick_request: the request that asks the router
// model to pick, and the allowed names. ok is false when the turn has no
// new text (a continuation), names no model, or has no cheaper rung to pick.
func (g *Gateway) PickRequest(body any) (req []byte, cands []string, ok bool) {
	task, err := latestUserText(body)
	if err != nil {
		return nil, nil, false
	}
	ask, err := newUserText(body)
	if err != nil {
		return nil, nil, false
	}
	requested, err := get(body, "model", "")
	if err != nil {
		return nil, nil, false
	}
	r, isStr := requested.(string)
	if pystr.Strip(ask) == "" || pystr.Strip(task) == "" || !isStr || r == "" {
		return nil, nil, false
	}
	cands = KnnCandidates(r, g.CheapModel, g.LocalModel, g.Prices)
	if len(cands) < 2 {
		return nil, nil, false
	}
	return PickerBody(g.RouterModel, task, cands), cands, true
}

// ask sends one short question through the upstream and returns the reply text.
func (s *Server) ask(url string, headers http.Header, body []byte) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), askTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(body)))
	if err != nil {
		return "", false
	}
	req.Header = headers.Clone()
	req.Header.Del("Content-Length")
	if _, has := req.Header["User-Agent"]; !has {
		req.Header["User-Agent"] = []string{"opendaisugi-gateway"}
	}
	req.ContentLength = int64(len(body))
	resp, err := s.Client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	content, err := io.ReadAll(decodedBody(resp))
	if err != nil || resp.StatusCode >= 400 {
		return "", false
	}
	v, err := LoadsBytes(content)
	if err != nil {
		return "", false
	}
	return ReplyText(v)
}

// pick asks the router model which allowed name to use. A nil result means
// the rules router decides without a reason from here (no text, no model).
func (s *Server) pick(gw *Gateway, url string, headers http.Header, body []byte) *Choice {
	v, err := LoadsBytes(body)
	if err != nil {
		return nil
	}
	req, cands, ok := gw.PickRequest(v)
	if !ok {
		return nil
	}
	text, got := s.ask(url, headers, req)
	if !got {
		return &Choice{Why: "the router model call failed"}
	}
	name := ParsePick(text, true, cands)
	if name == "" {
		return &Choice{Why: "the router model named a model that is not allowed"}
	}
	return &Choice{Name: name}
}

// grade runs after the reply is sent: it asks the grader model and writes
// the answer to the outcome table. It never blocks the turn.
func (s *Server) grade(gw *Gateway, rec Record, p *Prepared, answer, stop, url string, headers http.Header) {
	// Only a finished answer is graded: a tool call step is not one.
	if stop != "end_turn" || gw == nil || gw.GraderModel == "" || gw.Knn == nil || p == nil || p.Conversation == "" || answer == "" {
		return
	}
	go func() {
		// The governing ask is p.Task. The grade goes to the row booked for it: this
		// turn's row, or on a tool loop step the conversation's last booked row (RT-R-13a).
		turnID := rec.TurnID
		if turnID == "" {
			if pystr.Strip(p.Ask) != "" {
				return // a new ask whose booking failed has no row to grade
			}
			row, err := gw.Knn.Store.LastInConversation(p.Conversation)
			if err != nil || row == nil {
				return
			}
			turnID = row.TurnID
		}
		text, ok := s.ask(url, headers, GraderBody(gw.GraderModel, p.Task, answer))
		grade := ParseGrade(text, ok)
		if grade == "" {
			return
		}
		gw.Knn.Store.SetOutcome(turnID, grade, SourceGrader)
	}()
}
