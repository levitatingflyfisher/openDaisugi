package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// runEnvelope is coppice envelope propose|list|approve|decline. A steward
// proposes a project envelope; the owner approves it on the phone or the
// floor, or here. propose writes nothing: the server keeps it pending.
func (c *CLI) runEnvelope(remote string, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(c.Err, "envelope needs a verb: propose, list, approve or decline")
		return 1
	}
	sub, rest := argv[0], argv[1:]
	params := map[string]any{}
	cmd := "envelope." + sub
	switch sub {
	case "propose":
		if len(rest) < 2 {
			fmt.Fprintln(c.Err, "usage: coppice envelope propose PROJECT FILE [--tokens N] [--turns N] [--help-wait SECONDS] [--allow VERB]...\n"+
				"FILE holds the envelope object, or {\"envelope\": {...}, \"tokens\": N, \"turns\": N, \"allow\": [...]}.\n"+
				"--allow names a denied verb (\"git push\") the project may run; it always makes a card.")
			return 1
		}
		b, err := os.ReadFile(rest[1])
		if err != nil {
			fmt.Fprintf(c.Err, "cannot read %s: %v\n", rest[1], err)
			return 1
		}
		var wrap struct {
			Envelope  json.RawMessage `json:"envelope"`
			Tokens    int             `json:"tokens"`
			Turns     int             `json:"turns"`
			HelpWaitS int             `json:"help_wait_s"`
			Allow     []string        `json:"allow"`
		}
		env := json.RawMessage(b)
		if json.Unmarshal(b, &wrap) == nil && len(wrap.Envelope) > 0 {
			env = wrap.Envelope
			params["tokens"], params["turns"] = wrap.Tokens, wrap.Turns
			if wrap.HelpWaitS > 0 {
				params["help_wait_s"] = wrap.HelpWaitS
			}
			if len(wrap.Allow) > 0 {
				params["allow"] = wrap.Allow
			}
		}
		params["project"], params["envelope"] = rest[0], env
		for i := 2; i+1 < len(rest); i += 2 {
			if rest[i] == "--allow" {
				allow, _ := params["allow"].([]string)
				params["allow"] = append(allow, rest[i+1])
				continue
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil {
				fmt.Fprintf(c.Err, "%s needs a number\n", rest[i])
				return 1
			}
			switch rest[i] {
			case "--tokens":
				params["tokens"] = n
			case "--turns":
				params["turns"] = n
			case "--help-wait":
				params["help_wait_s"] = n
			default:
				fmt.Fprintf(c.Err, "unknown flag %s\n", rest[i])
				return 1
			}
		}
	case "list":
		cmd = "envelope.pending"
	case "approve", "decline":
		if len(rest) != 1 {
			fmt.Fprintf(c.Err, "usage: coppice envelope %s PROJECT\n", sub)
			return 1
		}
		params["project"] = filepath.Base(rest[0])
	default:
		fmt.Fprintf(c.Err, "unknown envelope verb %q. Known verbs: propose, list, approve, decline\n", sub)
		return 1
	}
	cl, code := c.dial(remote)
	if code != 0 {
		return code
	}
	defer cl.Close()
	if sub == "approve" {
		// Approve names the proposal it looked at: read the pending card
		// first and send its stamp, so a replacement made since is caught.
		pend, err := cl.Do("envelope.pending", map[string]any{})
		if err != nil {
			return c.reportDoErr(cl, err)
		}
		at := ""
		list, _ := pend["proposals"].([]any)
		for _, it := range list {
			if card, ok := it.(map[string]any); ok && card["project"] == params["project"] {
				at, _ = card["at"].(string)
			}
		}
		if at == "" {
			fmt.Fprintf(c.Err, "no pending envelope for %v. It may be approved, declined or replaced already.\n", params["project"])
			return 1
		}
		params["at"] = at
	}
	res, err := cl.Do(cmd, params)
	if err != nil {
		return c.reportDoErr(cl, err)
	}
	b, _ := json.Marshal(res)
	fmt.Fprintln(c.Out, string(b))
	return 0
}
